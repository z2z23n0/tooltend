package inventory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type Report struct {
	HostResult host.Result `json:"host_result"`
	Projects   []string    `json:"projects"`
}

type PersistResult struct {
	Projects     int `json:"projects"`
	Sources      int `json:"sources"`
	Components   int `json:"components"`
	Bindings     int `json:"bindings"`
	Dependencies int `json:"dependencies"`
}

func Scan(ctx context.Context, home, currentProject string, projects []string) (Report, error) {
	result, err := host.ScanAll(ctx, host.ScanOptions{HomeDir: home, CurrentProject: currentProject, Projects: projects})
	if err != nil {
		return Report{}, err
	}
	selected := append([]string(nil), projects...)
	if currentProject != "" {
		selected = append(selected, currentProject)
	}
	selected = uniquePaths(selected)
	return Report{HostResult: result, Projects: selected}, nil
}

func Persist(ctx context.Context, database *store.Store, report Report) (PersistResult, error) {
	if database == nil {
		return PersistResult{}, fmt.Errorf("inventory: store is required")
	}
	now := time.Now().UTC()
	result := PersistResult{}
	existingBindings, err := database.ListBindings(ctx, "")
	if err != nil {
		return result, err
	}
	existingByID := make(map[string]model.Binding, len(existingBindings))
	for _, binding := range existingBindings {
		existingByID[binding.ID] = binding
	}
	projectRoots := append([]string(nil), report.Projects...)
	for _, binding := range report.HostResult.Bindings {
		if binding.Project != "" {
			projectRoots = append(projectRoots, binding.Project)
		}
	}
	projectRoots = uniquePaths(projectRoots)
	projectIDs := make(map[string]string, len(projectRoots))
	for _, root := range projectRoots {
		fingerprint := digest(filepath.Clean(root))
		id := stableID("prj", fingerprint)
		value := model.Project{ID: id, RootPath: filepath.Clean(root), RootFingerprint: fingerprint, Selected: true, DiscoveredVia: "init", LastSeenAt: now}
		if err := database.UpsertProject(ctx, value); err != nil {
			return result, err
		}
		projectIDs[filepath.Clean(root)] = id
		result.Projects++
	}

	// A managed binding may appear to the host scanner as a local symlink into
	// ToolTend's generation root. Keep its adopted source identity instead of
	// manufacturing a second local component on every reconciliation.
	managedComponents := make(map[string]string)
	for _, binding := range report.HostResult.Bindings {
		observation := observationForKey(report.HostResult.Observations, binding.ComponentKey)
		installPath := observedInstallPath(binding, observation)
		bindingID := stableID("bnd", digest(string(binding.Host)+"\x00"+installPath))
		if existing, ok := existingByID[bindingID]; ok && existing.Managed {
			managedComponents[binding.ComponentKey] = existing.ComponentID
		}
	}

	componentIDs := make(map[string]string)
	manualDefaults := make(map[string]bool)
	observations := make(map[string]host.Observation, len(report.HostResult.Observations))
	for _, observation := range report.HostResult.Observations {
		observations[observation.Key] = observation
		if componentID := managedComponents[observation.Key]; componentID != "" {
			componentIDs[observation.Key] = componentID
			continue
		}
		source := sourceFromObservation(observation, now)
		if err := database.UpsertSource(ctx, source); err != nil {
			return result, err
		}
		result.Sources++
		componentKind := componentKind(observation.Kind)
		logicalKey := digest(string(componentKind) + "\x00" + source.IdentityHash)
		componentID := stableID("cmp", logicalKey)
		component := model.LogicalComponent{ID: componentID, Kind: componentKind, Name: observation.Name, SourceID: source.ID, LogicalKey: logicalKey, CreatedAt: now, UpdatedAt: now}
		if err := database.UpsertComponent(ctx, component); err != nil {
			return result, err
		}
		componentIDs[observation.Key] = componentID
		classification := classifyObservation(observation)
		manualDefaults[observation.Key] = classification != model.ClassificationClean ||
			source.Kind == model.SourceUnknown || source.Kind == model.SourceLocal || source.Kind == model.SourceHTTP ||
			source.Kind == model.SourceNPM || source.Kind == model.SourcePyPI || source.Kind == model.SourceHomebrew ||
			componentKind == model.ComponentHook || componentKind == model.ComponentHTTPMCP || componentKind == model.ComponentCLI
		result.Components++
	}

	type dependencyBindingCandidate struct {
		observation host.Observation
		dependency  host.DependencyRef
		componentID string
	}
	dependencyComponents := make(map[string]string)
	var dependencyBindings []dependencyBindingCandidate
	for _, observation := range report.HostResult.Observations {
		fromComponentID := componentIDs[observation.Key]
		if fromComponentID == "" {
			continue
		}
		for _, dependency := range observation.Dependencies {
			identity := strings.TrimSpace(dependency.PackageIdentity)
			if identity == "" || dependency.EvidencePath == "" {
				continue
			}
			toComponentID := dependencyComponents[identity]
			if toComponentID == "" {
				dependencyPath := dependencyInstallPath(dependency.InstallPath)
				if dependencyPath == "" {
					dependencyPath = dependency.EvidencePath
				}
				dependencyObservation := host.Observation{
					Kind: host.ComponentKind("cli"), Name: dependencyName(identity),
					Path: dependencyPath, Source: dependency.Source,
				}
				dependencySource := sourceFromObservation(dependencyObservation, now)
				if err := database.UpsertSource(ctx, dependencySource); err != nil {
					return result, err
				}
				logicalKey := digest(string(model.ComponentCLI) + "\x00" + dependencySource.IdentityHash)
				toComponentID = stableID("cmp", logicalKey)
				if err := database.UpsertComponent(ctx, model.LogicalComponent{
					ID: toComponentID, Kind: model.ComponentCLI, Name: dependencyName(identity), SourceID: dependencySource.ID,
					LogicalKey: logicalKey, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					return result, err
				}
				dependencyComponents[identity] = toComponentID
				result.Sources++
				result.Components++
			}
			dependencyID := stableID("dep", digest(fromComponentID+"\x00"+toComponentID+"\x00"+identity+"\x00"+dependency.Constraint+"\x00"+dependency.EvidencePath))
			if err := database.PutDependency(ctx, model.Dependency{
				ID: dependencyID, FromComponentID: fromComponentID, ToComponentID: toComponentID,
				PackageIdentity: identity, Constraint: dependency.Constraint,
				EvidencePath: dependency.EvidencePath, EvidenceLine: dependency.EvidenceLine, Explicit: true,
			}); err != nil {
				return result, err
			}
			result.Dependencies++
			if dependencyInstallPath(dependency.InstallPath) != "" {
				dependencyBindings = append(dependencyBindings, dependencyBindingCandidate{
					observation: observation, dependency: dependency, componentID: toComponentID,
				})
			}
		}
	}

	for _, binding := range report.HostResult.Bindings {
		componentID := componentIDs[binding.ComponentKey]
		if componentID == "" {
			continue
		}
		observation := observations[binding.ComponentKey]
		installPath := observedInstallPath(binding, observation)
		bindingID := stableID("bnd", digest(string(binding.Host)+"\x00"+installPath))
		projectID := ""
		if binding.Project != "" {
			projectID = projectIDs[filepath.Clean(binding.Project)]
		}
		value := model.Binding{
			ID:              bindingID,
			ComponentID:     componentID,
			Host:            hostKind(binding.Host),
			ProjectID:       projectID,
			Scope:           scopeKind(binding.Scope, projectID),
			InstallPath:     installPath,
			ConfigPath:      binding.ConfigPath,
			ConfigPointer:   observationPointer(observation),
			InstallMethod:   observedInstallMethod(observation),
			Managed:         false,
			Classification:  classifyObservation(observations[binding.ComponentKey]),
			ObservedVersion: observedVersion(observations[binding.ComponentKey]),
			LastSeenAt:      now,
		}
		if existing, ok := existingByID[bindingID]; ok && existing.Managed {
			value.ComponentID = existing.ComponentID
			value.ProjectID = existing.ProjectID
			value.Scope = existing.Scope
			value.InstallMethod = existing.InstallMethod
			value.ConfigPath = existing.ConfigPath
			value.ConfigPointer = existing.ConfigPointer
			value.Managed = true
			value.Classification = existing.Classification
			value.ObservedHash = existing.ObservedHash
			value.ObservedVersion = existing.ObservedVersion
			value.TrustHash = existing.TrustHash
			value.ActiveGenerationID = existing.ActiveGenerationID
		}
		if err := database.UpsertBinding(ctx, value); err != nil {
			return result, err
		}
		existingByID[bindingID] = value
		if _, err := database.GetPolicy(ctx, bindingID); errors.Is(err, sql.ErrNoRows) {
			policy := model.DefaultPolicy()
			policy.BindingID, policy.UpdatedAt = bindingID, now
			if manualDefaults[binding.ComponentKey] || managedComponents[binding.ComponentKey] != "" {
				policy.ApplyMode = model.ApplyManual
			}
			if err := database.SetPolicy(ctx, policy); err != nil {
				return result, err
			}
		} else if err != nil {
			return result, err
		}
		result.Bindings++
	}

	// An explicit dependency edge is evidence that an extension needs a CLI;
	// it is not evidence that the CLI is installed. Only a discovery-time,
	// absolute executable location becomes an observed binding. The binding
	// inherits the referring extension's host/scope/project provenance, while
	// SKILL.md remains dependency evidence only.
	persistedDependencyBindings := make(map[string]struct{})
	for _, candidate := range dependencyBindings {
		installPath := dependencyInstallPath(candidate.dependency.InstallPath)
		if installPath == "" {
			continue
		}
		bindingID := stableID("bnd", digest(string(candidate.observation.Host)+"\x00"+installPath))
		if _, alreadyPersisted := persistedDependencyBindings[bindingID]; alreadyPersisted {
			continue
		}
		persistedDependencyBindings[bindingID] = struct{}{}
		if existing, ok := existingByID[bindingID]; ok && existing.ComponentID != candidate.componentID {
			// Never steal a concrete host binding merely because another extension
			// mentions a package with a similarly named executable.
			continue
		}
		projectID := ""
		if candidate.observation.Project != "" {
			projectID = projectIDs[filepath.Clean(candidate.observation.Project)]
			if projectID == "" {
				continue
			}
		}
		expectedHost := hostKind(candidate.observation.Host)
		expectedScope := scopeKind(candidate.observation.Scope, projectID)
		if existing, ok := managedDependencyBinding(existingByID, candidate.componentID, expectedHost, expectedScope, projectID, candidate.dependency.Executable); ok && existing.ID != bindingID {
			existing.LastSeenAt = now
			if err := database.UpsertBinding(ctx, existing); err != nil {
				return result, err
			}
			existingByID[existing.ID] = existing
			result.Bindings++
			continue
		}
		version := strings.TrimSpace(candidate.dependency.Source.Version)
		classification := model.ClassificationUnknown
		if version != "" {
			classification = model.ClassificationClean
		}
		value := model.Binding{
			ID: bindingID, ComponentID: candidate.componentID,
			Host: expectedHost, ProjectID: projectID,
			Scope: expectedScope, InstallPath: installPath,
			InstallMethod: dependencyInstallMethod(candidate.dependency.Carrier),
			Managed:       false, Classification: classification, ObservedVersion: version, LastSeenAt: now,
		}
		if existing, ok := existingByID[bindingID]; ok && existing.Managed {
			value = existing
			value.LastSeenAt = now
		}
		if err := database.UpsertBinding(ctx, value); err != nil {
			return result, err
		}
		existingByID[bindingID] = value
		if _, err := database.GetPolicy(ctx, bindingID); errors.Is(err, sql.ErrNoRows) {
			policy := model.DefaultPolicy()
			policy.BindingID, policy.ApplyMode, policy.LocalCapMode, policy.UpdatedAt = bindingID, model.ApplyManual, model.ApplyManual, now
			if err := database.SetPolicy(ctx, policy); err != nil {
				return result, err
			}
		} else if err != nil {
			return result, err
		}
		result.Bindings++
	}
	return result, nil
}

func dependencyInstallPath(value string) string {
	if !filepath.IsAbs(value) || strings.ContainsRune(value, 0) {
		return ""
	}
	value = filepath.Clean(value)
	info, err := os.Stat(value)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return ""
	}
	return value
}

func dependencyInstallMethod(carrier string) string {
	switch carrier = strings.ToLower(strings.TrimSpace(carrier)); carrier {
	case "npm", "npx", "pipx", "uv", "uvx", "brew":
		return "observed-runtime:" + carrier
	default:
		return "observed-runtime"
	}
}

func managedDependencyBinding(values map[string]model.Binding, componentID string, hostValue model.HostKind, scope model.ScopeKind, projectID, executable string) (model.Binding, bool) {
	executable = filepath.Base(strings.TrimSpace(executable))
	var match model.Binding
	for _, value := range values {
		if !value.Managed || value.ComponentID != componentID || value.Host != hostValue || value.Scope != scope || value.ProjectID != projectID {
			continue
		}
		if executable != "" && filepath.Base(value.InstallPath) != executable {
			continue
		}
		if match.ID == "" || value.ID < match.ID {
			match = value
		}
	}
	return match, match.ID != ""
}

func observedInstallMethod(observation host.Observation) string {
	kind := sourceKind(observation.Source.Kind)
	if (observation.Kind == host.ComponentStdioMCP || observation.Kind == host.ComponentKind("cli")) &&
		(kind == model.SourceNPM || kind == model.SourcePyPI) {
		carrier := strings.ToLower(strings.TrimSpace(observation.Metadata["executable"]))
		if carrier != "" && !strings.ContainsAny(carrier, ":\x00\r\n") {
			return "observed-runtime:" + carrier
		}
		return "observed-runtime"
	}
	return "observed"
}

func dependencyName(identity string) string {
	if index := strings.IndexByte(identity, ':'); index >= 0 && index+1 < len(identity) {
		return identity[index+1:]
	}
	return identity
}

func observedInstallPath(binding host.Binding, observation host.Observation) string {
	if binding.InstallPath != "" && (binding.ConfigPath == "" || binding.InstallPath != binding.ConfigPath) {
		return binding.InstallPath
	}
	pointer := observationPointer(observation)
	if pointer == "" {
		pointer = binding.ComponentKey
	}
	return binding.ConfigPath + "#" + pointer
}

func observationPointer(observation host.Observation) string {
	if len(observation.Evidence) == 0 {
		return ""
	}
	return observation.Evidence[0].Pointer
}

func observationForKey(values []host.Observation, key string) host.Observation {
	for _, value := range values {
		if value.Key == key {
			return value
		}
	}
	return host.Observation{}
}

func classifyObservation(observation host.Observation) model.Classification {
	kind := sourceKind(observation.Source.Kind)
	switch kind {
	case model.SourceLocal:
		return model.ClassificationDetached
	case model.SourceUnknown:
		return model.ClassificationUnknown
	case model.SourceHTTP:
		return model.ClassificationClean
	default:
		if observedVersion(observation) != "" {
			return model.ClassificationClean
		}
		return model.ClassificationUnknown
	}
}

func observedVersion(observation host.Observation) string {
	for _, value := range []string{observation.Version, observation.Source.Version, observation.Source.Ref} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func sourceFromObservation(observation host.Observation, now time.Time) model.Source {
	kind := sourceKind(observation.Source.Kind)
	locator := canonicalLocator(kind, observation.Source.Locator)
	packageName := canonicalPackage(kind, observation.Source.Package)
	subdir := filepath.ToSlash(observation.Source.Subdir)
	switch kind {
	case model.SourceGit, model.SourceNPM, model.SourcePyPI, model.SourceHomebrew, model.SourceHTTP:
		canonical, err := adapter.CanonicalizeSource(adapter.SourceKind(kind), adapter.Source{
			Kind: adapter.SourceKind(kind), Locator: observation.Source.Locator,
			PackageName: observation.Source.Package, Subdir: observation.Source.Subdir,
		})
		if err != nil {
			kind, locator, packageName, subdir = model.SourceUnknown, "", "", ""
		} else {
			locator, packageName, subdir = canonical.Locator, canonical.PackageName, canonical.Subdir
		}
	}
	if locator == "" {
		locator = packageName
	}
	if locator == "" && observation.Path != "" {
		kind, locator = model.SourceUnknown, "unknown:"+digest(observation.Path)
	}
	identity := digest(string(kind) + "\x00" + locator + "\x00" + packageName + "\x00" + subdir)
	metadata := make(map[string]string, len(observation.Metadata)+2)
	for key, value := range observation.Metadata {
		metadata[key] = value
	}
	if observation.Legacy {
		metadata["observed_legacy"] = "true"
	}
	if observation.Source.Ref != "" {
		metadata["ref"] = observation.Source.Ref
	}
	encoded, _ := json.Marshal(metadata)
	return model.Source{
		ID:           stableID("src", identity),
		Kind:         kind,
		Locator:      locator,
		Subdir:       subdir,
		PackageName:  packageName,
		IdentityHash: identity,
		MetadataJSON: string(encoded),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func sourceKind(value string) model.SourceKind {
	switch strings.ToLower(value) {
	case "git", "github", "git-subdir":
		return model.SourceGit
	case "npm", "npx":
		return model.SourceNPM
	case "pypi", "pipx", "uv", "uvx", "python":
		return model.SourcePyPI
	case "homebrew", "brew":
		return model.SourceHomebrew
	case "http", "https", "remote":
		return model.SourceHTTP
	case "local", "path":
		return model.SourceLocal
	default:
		return model.SourceUnknown
	}
}

func componentKind(value host.ComponentKind) model.ComponentKind {
	switch value {
	case host.ComponentSkill:
		return model.ComponentSkill
	case host.ComponentPlugin:
		return model.ComponentPlugin
	case host.ComponentHook:
		return model.ComponentHook
	case host.ComponentStdioMCP:
		return model.ComponentStdioMCP
	case host.ComponentHTTPMCP:
		return model.ComponentHTTPMCP
	default:
		return model.ComponentCLI
	}
}

func hostKind(value host.Name) model.HostKind {
	if value == host.Claude {
		return model.HostClaude
	}
	return model.HostCodex
}

func scopeKind(value host.Scope, projectID string) model.ScopeKind {
	if projectID != "" || value == host.ScopeProject || value == host.ScopeLocal {
		return model.ScopeProject
	}
	return model.ScopeGlobal
}

func canonicalLocator(kind model.SourceKind, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if kind == model.SourceGit {
		if strings.HasPrefix(raw, "git@") && strings.Contains(raw, ":") {
			parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
			raw = "https://" + strings.ToLower(parts[0]) + "/" + parts[1]
		}
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			parsed.Host = strings.ToLower(parsed.Host)
			parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
			parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), ".git")
			if parsed.Scheme == "ssh" && parsed.Host == "github.com" {
				parsed.Scheme = "https"
			}
			return parsed.String()
		}
	}
	if kind == model.SourceHTTP {
		if parsed, err := url.Parse(raw); err == nil {
			parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
			return parsed.String()
		}
	}
	return raw
}

func canonicalPackage(kind model.SourceKind, raw string) string {
	raw = strings.TrimSpace(raw)
	if kind == model.SourceNPM || kind == model.SourcePyPI || kind == model.SourceHomebrew {
		raw = strings.ToLower(raw)
	}
	if kind == model.SourcePyPI {
		raw = strings.NewReplacer("_", "-", ".", "-").Replace(raw)
	}
	return raw
}

func stableID(prefix, hash string) string {
	if len(hash) > 26 {
		hash = hash[:26]
	}
	return prefix + "_" + hash
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func uniquePaths(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		value = filepath.Clean(value)
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
