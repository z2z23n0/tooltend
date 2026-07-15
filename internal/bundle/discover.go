package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type DiscoverOptions struct {
	HomeDir        string
	Executable     string
	BuildVersion   string
	LocalRecipeDir string
	LookupPath     func(string) (string, error)
	Now            func() time.Time
}

type DiscoverResult struct {
	Bundles          int            `json:"bundles"`
	Installations    int            `json:"installations"`
	ConsumerBindings int            `json:"consumer_bindings"`
	ByConfidence     map[string]int `json:"by_confidence"`
	Pruned           int64          `json:"pruned"`
}

type observedInstallation struct {
	component    model.LogicalComponent
	source       model.Source
	binding      model.Binding
	dependencies []model.Dependency
}

type matchedInstallation struct {
	path            string
	packageIdentity string
	sourceIdentity  string
	version         string
	hash            string
	consumers       []observedInstallation
	metadata        map[string]any
}

type skillSourceEvidence struct {
	SourceIdentity string
	ObservedHash   string
	Metadata       map[string]any
}

func Discover(ctx context.Context, database *store.Store, options DiscoverOptions) (DiscoverResult, error) {
	if database == nil {
		return DiscoverResult{}, errors.New("bundle discovery: store is required")
	}
	if options.HomeDir == "" {
		return DiscoverResult{}, errors.New("bundle discovery: home directory is required")
	}
	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	lookup := options.LookupPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	catalog, err := LoadCatalog(options.LocalRecipeDir)
	if err != nil {
		return DiscoverResult{}, err
	}
	observed, err := loadObservedInstallations(ctx, database)
	if err != nil {
		return DiscoverResult{}, err
	}
	result := DiscoverResult{ByConfidence: map[string]int{}}
	skillEvidence := loadSkillSourceEvidence(options.HomeDir)
	matchedBindings := map[string]struct{}{}
	for _, recipe := range catalog.Recipes() {
		matches := make(map[string][]matchedInstallation, len(recipe.Artifacts))
		for _, artifact := range recipe.Artifacts {
			for index := range observed {
				item := &observed[index]
				if observedHostOwned(*item) && recipe.Owner != model.LifecycleHostOwned {
					continue
				}
				if artifactMatches(artifact, *item) {
					match := matchedFromObserved(*item, artifact, options.HomeDir)
					enrichSkillMatch(&match, *item, skillEvidence)
					enrichWorkspaceMatch(&match, *item)
					matches[artifact.Key] = append(matches[artifact.Key], match)
					matchedBindings[item.binding.ID] = struct{}{}
				}
			}
			for _, probe := range artifact.Probes {
				match, ok := resolveProbe(ctx, probe, recipe, artifact, options, lookup)
				if ok {
					matches[artifact.Key] = append(matches[artifact.Key], match)
				}
			}
			matches[artifact.Key] = dedupeMatches(matches[artifact.Key])
		}
		if countMatches(matches) == 0 {
			continue
		}
		confidence := effectiveConfidence(recipe, matches)
		if err := persistRecipeMatch(ctx, database, recipe, confidence, matches, now, &result); err != nil {
			return result, err
		}
	}
	if err := persistFallbacks(ctx, database, observed, matchedBindings, now, options.HomeDir, &result); err != nil {
		return result, err
	}
	prunedInstallations, err := database.PruneUnconfiguredInstallations(ctx, now)
	if err != nil {
		return result, err
	}
	prunedBundles, err := database.PruneUnconfiguredBundles(ctx, now)
	if err != nil {
		return result, err
	}
	result.Pruned = prunedInstallations + prunedBundles
	if err := refreshDiscoveryCounts(ctx, database, &result); err != nil {
		return result, err
	}
	return result, nil
}

func observedHostOwned(value observedInstallation) bool {
	return value.binding.HostOwned() || strings.Contains(filepath.ToSlash(value.binding.InstallPath), "/.codex/plugins/cache/")
}

func refreshDiscoveryCounts(ctx context.Context, database *store.Store, result *DiscoverResult) error {
	for target, query := range map[*int]string{
		&result.Bundles:          `SELECT COUNT(*) FROM bundles`,
		&result.Installations:    `SELECT COUNT(*) FROM installations`,
		&result.ConsumerBindings: `SELECT COUNT(*) FROM consumer_bindings`,
	} {
		if err := database.DB().QueryRowContext(ctx, query).Scan(target); err != nil {
			return err
		}
	}
	result.ByConfidence = map[string]int{}
	rows, err := database.DB().QueryContext(ctx, `SELECT confidence,COUNT(*) FROM bundles GROUP BY confidence`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var confidence string
		var count int
		if err := rows.Scan(&confidence, &count); err != nil {
			return err
		}
		result.ByConfidence[confidence] = count
	}
	return rows.Err()
}

func loadObservedInstallations(ctx context.Context, database *store.Store) ([]observedInstallation, error) {
	components, err := database.ListComponents(ctx)
	if err != nil {
		return nil, err
	}
	dependencies, err := database.ListDependencies(ctx)
	if err != nil {
		return nil, err
	}
	depsByComponent := make(map[string][]model.Dependency)
	for _, dependency := range dependencies {
		depsByComponent[dependency.FromComponentID] = append(depsByComponent[dependency.FromComponentID], dependency)
	}
	var result []observedInstallation
	for _, component := range components {
		var source model.Source
		if component.SourceID != "" {
			source, err = database.GetSource(ctx, component.SourceID)
			if err != nil {
				return nil, err
			}
		}
		bindings, err := database.ListBindings(ctx, component.ID)
		if err != nil {
			return nil, err
		}
		for _, binding := range bindings {
			result = append(result, observedInstallation{component: component, source: source, binding: binding, dependencies: depsByComponent[component.ID]})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].binding.ID < result[j].binding.ID })
	return result, nil
}

func artifactMatches(recipe ArtifactRecipe, value observedInstallation) bool {
	if len(recipe.Selectors) == 0 {
		return false
	}
	for _, selector := range recipe.Selectors {
		var candidates []string
		switch selector.Field {
		case "name":
			candidates = []string{value.component.Name}
		case "kind":
			candidates = []string{string(value.component.Kind)}
		case "path":
			candidates = []string{value.binding.InstallPath, value.binding.ConfigPath}
		case "package":
			candidates = []string{value.source.PackageName}
		case "source":
			candidates = []string{value.source.Locator}
		case "dependency":
			for _, dependency := range value.dependencies {
				candidates = append(candidates, dependency.PackageIdentity)
			}
		case "host":
			candidates = []string{string(value.binding.Host)}
		}
		if !selectorMatches(selector, candidates) {
			return false
		}
	}
	return true
}

func selectorMatches(selector Selector, candidates []string) bool {
	for _, candidate := range candidates {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if selector.Equals != "" {
			for _, alternative := range strings.Split(strings.ToLower(selector.Equals), "|") {
				if candidate == strings.TrimSpace(alternative) {
					return true
				}
			}
		} else if strings.Contains(candidate, strings.ToLower(selector.Contains)) {
			return true
		}
	}
	return false
}

func matchedFromObserved(value observedInstallation, artifact ArtifactRecipe, home string) matchedInstallation {
	path := normalizeInstallationPath(value.binding.InstallPath, home)
	packageIdentity := value.source.PackageName
	if packageIdentity == "" {
		packageIdentity = value.component.Name
	}
	version, observedHash := actualInstalledVersion(path, packageIdentity, value.binding.ObservedVersion)
	if observedHash == "" {
		observedHash = value.binding.ObservedHash
	}
	return matchedInstallation{
		path: path, packageIdentity: packageIdentity, sourceIdentity: value.source.IdentityHash,
		version: version, hash: observedHash, consumers: []observedInstallation{value},
		metadata: map[string]any{"legacy_component_id": value.component.ID, "legacy_binding_id": value.binding.ID, "artifact_driver": artifact.Driver},
	}
}

func resolveProbe(ctx context.Context, probe string, recipe Recipe, artifact ArtifactRecipe, options DiscoverOptions, lookup func(string) (string, error)) (matchedInstallation, bool) {
	kind, value, ok := strings.Cut(probe, ":")
	if !ok {
		return matchedInstallation{}, false
	}
	var path string
	switch kind {
	case "command":
		path, _ = lookup(value)
		if value == "tooltend" && options.Executable != "" {
			path = options.Executable
		}
	case "path":
		path = expandHome(value, options.HomeDir)
		if info, err := os.Lstat(path); err != nil || (!info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0) {
			path = ""
		}
	}
	if path == "" {
		return matchedInstallation{}, false
	}
	path = normalizeInstallationPath(path, options.HomeDir)
	packageIdentity := recipe.ID
	version, observedHash := "", ""
	metadata := map[string]any{"probe": probe}
	if artifact.Driver == "npm" {
		if packageName, packageVersion, packageHash := nearestPackageMetadata(path); packageName != "" {
			packageIdentity, version, observedHash = packageName, packageVersion, packageHash
			metadata["npm_package"] = packageName
		}
	}
	if recipe.ID == "tooltend" && options.BuildVersion != "" && options.BuildVersion != "dev" {
		version = strings.TrimPrefix(options.BuildVersion, "v")
	}
	if artifact.Kind == model.ArtifactApp {
		if appVersion, appMetadata := inspectApp(path); appVersion != "" {
			version = appVersion
			for key, item := range appMetadata {
				metadata[key] = item
			}
		}
		metadata["code_signature_valid"] = verifyAppSignature(ctx, path)
	}
	return matchedInstallation{path: path, packageIdentity: packageIdentity, version: version, hash: observedHash, metadata: metadata}, true
}

func persistRecipeMatch(ctx context.Context, database *store.Store, recipe Recipe, confidence model.BundleConfidence, matches map[string][]matchedInstallation, now time.Time, result *DiscoverResult) error {
	bundleID := stableID("bun", recipe.ID)
	metadata, _ := json.Marshal(map[string]any{"description": recipe.Description})
	value := model.Bundle{
		ID: bundleID, Slug: recipe.ID, Name: recipe.Name, RecipeID: recipe.ID, RecipeVersion: recipe.Version,
		RecipeSource: recipe.Source, Owner: recipe.Owner, ConfigState: model.BundleUnconfigured, Confidence: confidence,
		MetadataJSON: string(metadata), DiscoveredAt: now, LastSeenAt: now,
	}
	if err := database.UpsertBundle(ctx, value); err != nil {
		return err
	}
	result.Bundles++
	result.ByConfidence[string(confidence)]++
	for ordinal, artifactRecipe := range recipe.Artifacts {
		artifactID := stableID("art", bundleID+"\x00"+artifactRecipe.Key)
		artifactMetadata, _ := json.Marshal(artifactRecipe)
		artifact := model.BundleArtifact{
			ID: artifactID, BundleID: bundleID, RecipeKey: artifactRecipe.Key, Kind: artifactRecipe.Kind,
			Name: artifactRecipe.Name, Ordinal: ordinal, Required: artifactRecipe.Required, Driver: artifactRecipe.Driver,
			MetadataJSON: string(artifactMetadata),
		}
		if err := database.UpsertBundleArtifact(ctx, artifact); err != nil {
			return err
		}
		for _, match := range matches[artifactRecipe.Key] {
			if err := persistInstallation(ctx, database, value, artifact, match, now, result); err != nil {
				return err
			}
		}
	}
	return persistObservedRelease(ctx, database, value, matches, now)
}

func persistObservedRelease(ctx context.Context, database *store.Store, bundle model.Bundle, matches map[string][]matchedInstallation, now time.Time) error {
	versions := map[string][]string{}
	for key, values := range matches {
		for _, value := range values {
			if value.version != "" {
				versions[key] = append(versions[key], value.version)
			}
		}
		sort.Strings(versions[key])
	}
	if len(versions) == 0 {
		return nil
	}
	manifest, _ := json.Marshal(map[string]any{"artifacts": versions})
	version := ""
	var flattened []string
	for _, values := range versions {
		flattened = append(flattened, values...)
	}
	sort.Strings(flattened)
	if len(flattened) > 0 {
		version = flattened[0]
		for _, candidate := range flattened[1:] {
			if candidate != version {
				digest := sha256.Sum256(manifest)
				version = "observed-" + hex.EncodeToString(digest[:6])
				break
			}
		}
	}
	releaseID := stableID("rel", bundle.ID+"\x00"+string(manifest))
	if err := database.UpsertBundleRelease(ctx, model.BundleRelease{
		ID: releaseID, BundleID: bundle.ID, Version: version, ManifestJSON: string(manifest), Status: "observed", CreatedAt: now,
	}); err != nil {
		return err
	}
	return database.SetBundleCurrentRelease(ctx, bundle.ID, releaseID)
}

func persistInstallation(ctx context.Context, database *store.Store, bundle model.Bundle, artifact model.BundleArtifact, match matchedInstallation, now time.Time, result *DiscoverResult) error {
	metadata, _ := json.Marshal(match.metadata)
	material := strings.Join([]string{artifact.Driver, match.path, match.packageIdentity, match.sourceIdentity}, "\x00")
	installationID := stableID("ins", material)
	installation := model.Installation{
		ID: installationID, BundleID: bundle.ID, ArtifactID: artifact.ID, Driver: artifact.Driver, Path: match.path,
		PackageIdentity: match.packageIdentity, SourceIdentity: match.sourceIdentity, ObservedVersion: match.version,
		ObservedHash: match.hash, Owner: bundle.Owner, MetadataJSON: string(metadata), LastSeenAt: now,
	}
	if err := database.UpsertInstallation(ctx, installation); err != nil {
		return err
	}
	result.Installations++
	for _, observed := range match.consumers {
		legacy := observed.binding
		consumer := model.ConsumerBinding{
			ID: stableID("con", installationID+"\x00"+legacy.ID), InstallationID: installationID, BindingID: legacy.ID,
			Host: legacy.Host, ProjectID: legacy.ProjectID, Scope: legacy.Scope, ConfigPath: legacy.ConfigPath,
			ConfigPointer: legacy.ConfigPointer, LastSeenAt: now,
		}
		if err := database.UpsertConsumerBinding(ctx, consumer); err != nil {
			return err
		}
		result.ConsumerBindings++
	}
	return nil
}

func persistFallbacks(ctx context.Context, database *store.Store, observed []observedInstallation, matched map[string]struct{}, now time.Time, home string, result *DiscoverResult) error {
	type fallbackGroup struct {
		bundle model.Bundle
		items  []observedInstallation
	}
	groups := map[string]*fallbackGroup{}
	for _, item := range observed {
		if _, ok := matched[item.binding.ID]; ok {
			continue
		}
		owner := model.LifecycleUnresolved
		confidence := model.BundleConfidenceUnresolved
		groupKey := "unresolved:" + item.component.LogicalKey
		name := item.component.Name
		if item.binding.HostOwned() || strings.Contains(item.binding.InstallPath, filepath.Join(".codex", "plugins", "cache")) {
			owner, confidence = model.LifecycleHostOwned, model.BundleConfidenceMedium
			plugin := pluginGroup(item.binding.InstallPath)
			if plugin != "" {
				groupKey, name = "host-plugin:"+plugin, plugin+" (host-owned)"
			}
		} else if linkedWorkspace(item.binding.InstallPath, item.source.Locator, home) {
			owner, confidence = model.LifecycleWorkspaceLinked, model.BundleConfidenceMedium
			groupKey = "workspace:" + item.component.LogicalKey
		}
		slug := fallbackSlug(name, groupKey)
		group := groups[groupKey]
		if group == nil {
			group = &fallbackGroup{bundle: model.Bundle{
				ID: stableID("bun", groupKey), Slug: slug, Name: name, RecipeID: "fallback", RecipeVersion: "1", RecipeSource: "fallback",
				Owner: owner, ConfigState: model.BundleUnconfigured, Confidence: confidence, MetadataJSON: "{}", DiscoveredAt: now, LastSeenAt: now,
			}}
			groups[groupKey] = group
		}
		group.items = append(group.items, item)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		if err := database.UpsertBundle(ctx, group.bundle); err != nil {
			return err
		}
		result.Bundles++
		result.ByConfidence[string(group.bundle.Confidence)]++
		artifactByComponent := map[string]model.BundleArtifact{}
		for _, item := range group.items {
			artifact := artifactByComponent[item.component.ID]
			if artifact.ID == "" {
				kind := artifactKind(item.component.Kind)
				artifact = model.BundleArtifact{
					ID: stableID("art", group.bundle.ID+"\x00"+item.component.ID), BundleID: group.bundle.ID,
					RecipeKey: item.component.ID, Kind: kind, Name: item.component.Name, Ordinal: len(artifactByComponent),
					Required: true, Driver: "observe", MetadataJSON: "{}",
				}
				if err := database.UpsertBundleArtifact(ctx, artifact); err != nil {
					return err
				}
				artifactByComponent[item.component.ID] = artifact
			}
			match := matchedFromObserved(item, ArtifactRecipe{Driver: "observe"}, home)
			enrichWorkspaceMatch(&match, item)
			if err := persistInstallation(ctx, database, group.bundle, artifact, match, now, result); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadSkillSourceEvidence(home string) map[string]skillSourceEvidence {
	result := map[string]skillSourceEvidence{}
	loadSkillLock(filepath.Join(home, ".agents", ".skill-lock.json"), result)
	for _, root := range []string{filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".codex", "skills"), filepath.Join(home, ".claude", "skills")} {
		loadMTSkillsRecords(filepath.Join(root, ".mtskills-source.jsonl"), result)
	}
	return result
}

func loadSkillLock(path string, destination map[string]skillSourceEvidence) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var lock struct {
		Version int `json:"version"`
		Skills  map[string]struct {
			Source          string `json:"source"`
			SourceType      string `json:"sourceType"`
			SourceURL       string `json:"sourceUrl"`
			SkillPath       string `json:"skillPath"`
			SkillFolderHash string `json:"skillFolderHash"`
			InstalledAt     string `json:"installedAt"`
			UpdatedAt       string `json:"updatedAt"`
		} `json:"skills"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return
	}
	for name, item := range lock.Skills {
		destination[strings.ToLower(name)] = skillSourceEvidence{SourceIdentity: strings.TrimSpace(item.SourceURL) + "#" + strings.TrimSpace(item.SkillPath),
			ObservedHash: item.SkillFolderHash, Metadata: map[string]any{"lifecycle_manager": "npx-skills", "lock_version": lock.Version,
				"source": item.Source, "source_type": item.SourceType, "source_url": item.SourceURL, "skill_path": item.SkillPath,
				"installed_at": item.InstalledAt, "updated_at": item.UpdatedAt}}
	}
}

func loadMTSkillsRecords(path string, destination map[string]skillSourceEvidence) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record struct {
			SkillName   string `json:"skillName"`
			SourceType  string `json:"sourceType"`
			SourceID    string `json:"sourceId"`
			Environment string `json:"env"`
			Version     string `json:"version"`
			InstalledAt string `json:"installedAt"`
			TargetDir   string `json:"targetDir"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || record.SkillName == "" {
			continue
		}
		skillPath := filepath.Join(record.TargetDir, record.SkillName)
		manifestID, integrity, signaturePresent := inspectSignedSkillManifest(skillPath)
		metadata := map[string]any{"lifecycle_manager": "mtskills", "source_type": record.SourceType, "source_id": record.SourceID,
			"environment": record.Environment, "installed_at": record.InstalledAt, "target_dir": record.TargetDir,
			"manifest_integrity_valid": integrity, "signature_present": signaturePresent}
		if exactVersion(record.Version) {
			metadata["source_version"] = strings.TrimPrefix(record.Version, "v")
		}
		if manifestID != "" {
			metadata["skill_id"] = manifestID
		}
		destination[strings.ToLower(record.SkillName)] = skillSourceEvidence{SourceIdentity: "mtskills:" + record.Environment + ":" + record.SourceID,
			ObservedHash: manifestID, Metadata: metadata}
	}
}

func enrichSkillMatch(match *matchedInstallation, observed observedInstallation, evidence map[string]skillSourceEvidence) {
	if observed.component.Kind != model.ComponentSkill {
		return
	}
	value, ok := evidence[strings.ToLower(observed.component.Name)]
	if !ok {
		return
	}
	if value.SourceIdentity != "" {
		match.sourceIdentity = value.SourceIdentity
	}
	if value.ObservedHash != "" {
		match.hash = value.ObservedHash
	}
	for key, item := range value.Metadata {
		match.metadata[key] = item
	}
}

func enrichWorkspaceMatch(match *matchedInstallation, observed observedInstallation) {
	path := observed.binding.InstallPath
	if path == "" {
		path = observed.source.Locator
	}
	if commit := readGitCommit(path); commit != "" {
		match.metadata["git_commit"] = commit
		if match.version == "" {
			match.hash = commit
		}
	}
}

func inspectSignedSkillManifest(skillPath string) (string, bool, bool) {
	manifestPath := filepath.Join(skillPath, "skill.manifest")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", false, false
	}
	manifestID := ""
	integrity := true
	manifestFiles := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "skill-id:") {
			manifestID = strings.TrimSpace(strings.TrimPrefix(trimmed, "skill-id:"))
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			continue
		}
		manifestFiles++
		content, readErr := os.ReadFile(filepath.Join(skillPath, filepath.FromSlash(fields[1])))
		if readErr != nil {
			integrity = false
			continue
		}
		digest := sha256.Sum256(content)
		if !strings.EqualFold(fields[0], hex.EncodeToString(digest[:])) {
			integrity = false
		}
	}
	signature, err := os.Stat(filepath.Join(skillPath, "skill.sig"))
	return manifestID, integrity && manifestID != "" && manifestFiles > 0, err == nil && signature.Mode().IsRegular() && signature.Size() > 0
}

func readGitCommit(path string) string {
	root := path
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		root = filepath.Dir(root)
	}
	for {
		gitPath := filepath.Join(root, ".git")
		if info, err := os.Stat(gitPath); err == nil {
			gitDir := gitPath
			if !info.IsDir() {
				data, readErr := os.ReadFile(gitPath)
				if readErr != nil || !strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:") {
					return ""
				}
				gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(root, gitDir)
				}
			}
			head, readErr := os.ReadFile(filepath.Join(gitDir, "HEAD"))
			if readErr != nil {
				return ""
			}
			value := strings.TrimSpace(string(head))
			if !strings.HasPrefix(value, "ref:") {
				return validGitHash(value)
			}
			ref := strings.TrimSpace(strings.TrimPrefix(value, "ref:"))
			if data, readErr := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); readErr == nil {
				return validGitHash(strings.TrimSpace(string(data)))
			}
			packed, _ := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
			for _, line := range strings.Split(string(packed), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[1] == ref {
					return validGitHash(fields[0])
				}
			}
			return ""
		}
		parent := filepath.Dir(root)
		if parent == root {
			return ""
		}
		root = parent
	}
}

func validGitHash(value string) string {
	if len(value) != 40 && len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

func verifyAppSignature(ctx context.Context, path string) bool {
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, "codesign", "--verify", "--deep", "--strict", path)
	command.Stdout, command.Stderr = nil, nil
	return command.Run() == nil
}

func effectiveConfidence(recipe Recipe, matches map[string][]matchedInstallation) model.BundleConfidence {
	for _, artifact := range recipe.Artifacts {
		if artifact.Required && len(matches[artifact.Key]) == 0 {
			if recipe.Confidence == model.BundleConfidenceHigh {
				return model.BundleConfidenceMedium
			}
			return recipe.Confidence
		}
	}
	return recipe.Confidence
}

func countMatches(values map[string][]matchedInstallation) int {
	total := 0
	for _, matches := range values {
		total += len(matches)
	}
	return total
}

func dedupeMatches(values []matchedInstallation) []matchedInstallation {
	result := make([]matchedInstallation, 0, len(values))
	for _, value := range values {
		merged := false
		for index := range result {
			if samePhysicalEvidence(result[index], value) {
				result[index] = mergeMatchEvidence(result[index], value)
				merged = true
				break
			}
		}
		if !merged {
			result = append(result, value)
		}
	}
	return result
}

func samePhysicalEvidence(left, right matchedInstallation) bool {
	if left.path == "" || left.path != right.path {
		return false
	}
	if left.packageIdentity == right.packageIdentity && left.sourceIdentity == right.sourceIdentity {
		return true
	}
	// A command/path probe is weaker evidence for an already observed path. It
	// must enrich that physical installation instead of creating a second row
	// merely because the probe cannot know the source identity.
	return probeMatch(left) || probeMatch(right)
}

func mergeMatchEvidence(left, right matchedInstallation) matchedInstallation {
	if probeMatch(left) && !probeMatch(right) {
		left, right = right, left
	}
	if left.packageIdentity == "" {
		left.packageIdentity = right.packageIdentity
	}
	if left.sourceIdentity == "" {
		left.sourceIdentity = right.sourceIdentity
	}
	if left.version == "" {
		left.version = right.version
	}
	if left.hash == "" {
		left.hash = right.hash
	}
	left.consumers = append(left.consumers, right.consumers...)
	if left.metadata == nil {
		left.metadata = map[string]any{}
	}
	for key, value := range right.metadata {
		if _, exists := left.metadata[key]; !exists {
			left.metadata[key] = value
		}
	}
	return left
}

func probeMatch(value matchedInstallation) bool {
	_, ok := value.metadata["probe"]
	return ok
}

func normalizeInstallationPath(path, home string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = expandHome(path, home)
	if strings.Contains(path, "#") {
		return filepath.Clean(path)
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = filepath.Clean(resolved)
	}
	return path
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func actualInstalledVersion(path, packageIdentity, observed string) (string, string) {
	if packageIdentity != "" && path != "" {
		cursor := path
		if info, err := os.Stat(cursor); err == nil && !info.IsDir() {
			cursor = filepath.Dir(cursor)
		}
		for depth := 0; depth < 8; depth++ {
			data, err := os.ReadFile(filepath.Join(cursor, "package.json"))
			if err == nil {
				var pkg struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				}
				if json.Unmarshal(data, &pkg) == nil && pkg.Name == packageIdentity && exactVersion(pkg.Version) {
					hash := sha256.Sum256(data)
					return pkg.Version, hex.EncodeToString(hash[:])
				}
			}
			parent := filepath.Dir(cursor)
			if parent == cursor {
				break
			}
			cursor = parent
		}
	}
	if exactVersion(observed) {
		return strings.TrimPrefix(strings.TrimSpace(observed), "v"), ""
	}
	return "", ""
}

func nearestPackageMetadata(path string) (string, string, string) {
	cursor := path
	if info, err := os.Stat(cursor); err == nil && !info.IsDir() {
		cursor = filepath.Dir(cursor)
	}
	for depth := 0; depth < 8; depth++ {
		data, err := os.ReadFile(filepath.Join(cursor, "package.json"))
		if err == nil {
			var pkg struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &pkg) == nil && strings.TrimSpace(pkg.Name) != "" {
				digest := sha256.Sum256(data)
				version := ""
				if exactVersion(pkg.Version) {
					version = strings.TrimPrefix(strings.TrimSpace(pkg.Version), "v")
				}
				return strings.TrimSpace(pkg.Name), version, hex.EncodeToString(digest[:])
			}
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
		cursor = parent
	}
	return "", "", ""
}

var semverLike = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

func exactVersion(value string) bool {
	value = strings.TrimSpace(value)
	return semverLike.MatchString(value)
}

func inspectApp(path string) (string, map[string]any) {
	data, err := os.ReadFile(filepath.Join(path, "Contents", "Info.plist"))
	if err != nil {
		return "", nil
	}
	text := string(data)
	value := plistString(text, "CFBundleShortVersionString")
	metadata := map[string]any{
		"bundle_identifier": plistString(text, "CFBundleIdentifier"),
		"sparkle_feed":      plistString(text, "SUFeedURL"),
		"sparkle_enabled":   strings.Contains(text, "<key>SUAutomaticallyUpdate</key>") && strings.Contains(text, "<true/>") || plistString(text, "SUAutomaticallyUpdate") == "1",
	}
	return value, metadata
}

func plistString(data, key string) string {
	needle := "<key>" + key + "</key>"
	index := strings.Index(data, needle)
	if index < 0 {
		return ""
	}
	rest := data[index+len(needle):]
	start := strings.Index(rest, "<string>")
	end := strings.Index(rest, "</string>")
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return strings.TrimSpace(rest[start+len("<string>") : end])
}

func pluginGroup(path string) string {
	path = filepath.ToSlash(path)
	marker := "/.codex/plugins/cache/"
	index := strings.Index(path, marker)
	if index < 0 {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(path[index+len(marker):], "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	for _, part := range parts[1:] {
		if part == "skills" || semverLike.MatchString(part) || strings.Contains(part, ".") || len(part) >= 7 && isHex(part) {
			continue
		}
		return part
	}
	return parts[1]
}

func linkedWorkspace(path, locator, home string) bool {
	for _, value := range []string{path, locator} {
		if strings.Contains(filepath.Clean(value), filepath.Join(home, "workspace")) {
			return true
		}
	}
	return false
}

func fallbackSlug(name, key string) string {
	base := strings.ToLower(name)
	var b strings.Builder
	for _, char := range base {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			b.WriteRune(char)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	base = strings.Trim(b.String(), "-")
	if base == "" {
		base = "bundle"
	}
	digest := sha256.Sum256([]byte(key))
	return base + "-" + hex.EncodeToString(digest[:4])
}

func artifactKind(kind model.ComponentKind) model.ArtifactKind {
	switch kind {
	case model.ComponentCLI:
		return model.ArtifactCLI
	case model.ComponentSkill:
		return model.ArtifactSkill
	case model.ComponentHook:
		return model.ArtifactHook
	case model.ComponentPlugin:
		return model.ArtifactPlugin
	default:
		return model.ArtifactMCP
	}
}

func stableID(prefix, material string) string {
	digest := sha256.Sum256([]byte(material))
	return prefix + "_" + hex.EncodeToString(digest[:13])
}

func isHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func formatError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
