package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/merge"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/policy"
)

const validationRulesVersion = "tooltend-lifecycle-v1"

type candidateIdentity struct {
	BindingID   string `json:"binding_id"`
	SourceID    string `json:"source_id"`
	ResolvedRef string `json:"resolved_ref"`
	Upstream    string `json:"upstream_tree_hash,omitempty"`
	Baseline    string `json:"baseline_id,omitempty"`
	Overlay     string `json:"overlay_id,omitempty"`
	Rules       string `json:"validation_rules"`
	Operation   string `json:"operation"`
}

func stableHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// capturedTreeIntegrity derives the activation hash from the immutable object
// tree, not from a second traversal of the live generation. The activation
// manager subsequently compares this hash with the live old generation before
// and after switching, so an edit made after CaptureTree cannot be silently
// paired with an older TreeHash.
func (s *Service) capturedTreeIntegrity(ctx context.Context, treeHash string, runtimeComponent bool) (string, error) {
	if err := os.MkdirAll(s.Paths.StagingDir, 0o700); err != nil {
		return "", err
	}
	holder, err := os.MkdirTemp(s.Paths.StagingDir, ".snapshot-integrity-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(holder)
	snapshot := filepath.Join(holder, "generation")
	if err := s.Objects.MaterializeTree(ctx, treeHash, snapshot); err != nil {
		return "", err
	}
	if runtimeComponent {
		return activation.HashRuntimeGeneration(snapshot)
	}
	return activation.HashGeneration(snapshot)
}

func stableCandidateID(hash string) string {
	if len(hash) > 24 {
		hash = hash[:24]
	}
	return "cand_" + hash
}

func (s *Service) runtimeShimPath(component model.LogicalComponent, binding model.Binding, executableName string) (string, error) {
	name := filepath.Base(filepath.Clean(executableName))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "", errors.New("lifecycle: runtime executable name is invalid")
	}
	if component.Kind == model.ComponentStdioMCP {
		hash, err := stableHash(struct {
			BindingID  string `json:"binding_id"`
			Executable string `json:"executable"`
		}{binding.ID, name})
		if err != nil {
			return "", err
		}
		// stdio MCP configurations can point directly at a binding-specific
		// absolute shim. Sharing the natural CLI name would couple Codex and
		// Claude bindings to whichever runtime was adopted last.
		name += "-tooltend-" + hash[:12]
	}
	return filepath.Join(s.Paths.ShimDir, name), nil
}

func ensureCLIShimPrecedence(shimPath, pathEnv string) error {
	shimDir := canonicalPath(filepath.Dir(shimPath))
	name := filepath.Base(shimPath)
	foundShimDir := false
	for _, raw := range filepath.SplitList(pathEnv) {
		directory := strings.TrimSpace(raw)
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		if canonicalPath(directory) == shimDir {
			foundShimDir = true
			break
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return fmt.Errorf("lifecycle: managed shim %s would be shadowed by %s earlier in PATH", shimPath, candidate)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("lifecycle: inspect PATH candidate %s: %w", candidate, err)
		}
	}
	if !foundShimDir {
		return fmt.Errorf("lifecycle: runtime shim directory %s is not in PATH", filepath.Dir(shimPath))
	}
	return nil
}

func canonicalPath(value string) string {
	value = filepath.Clean(value)
	if resolved, err := filepath.EvalSymlinks(value); err == nil {
		return filepath.Clean(resolved)
	}
	return value
}

func (s *Service) candidateByHash(ctx context.Context, bindingID, hash string) (model.UpdateCandidate, bool, error) {
	values, err := s.Database.ListCandidates(ctx, bindingID, "")
	if err != nil {
		return model.UpdateCandidate{}, false, err
	}
	for _, value := range values {
		if value.CandidateHash == hash {
			return value, true, nil
		}
	}
	return model.UpdateCandidate{}, false, nil
}

func (s *Service) putAvailableCandidate(ctx context.Context, value model.UpdateCandidate) (model.UpdateCandidate, error) {
	if existing, ok, err := s.candidateByHash(ctx, value.BindingID, value.CandidateHash); err != nil {
		return model.UpdateCandidate{}, err
	} else if ok {
		if retryableCandidateState(existing.Status) {
			if err := s.Database.ResetCandidateForRetry(ctx, existing.ID); err != nil {
				return model.UpdateCandidate{}, err
			}
			return s.Database.GetCandidate(ctx, existing.ID)
		}
		return existing, nil
	}
	value.Status = model.CandidateAvailable
	value.ReviewClass = model.ReviewNone
	value.CreatedAt = s.now()
	value.UpdatedAt = value.CreatedAt
	if value.ID == "" {
		value.ID = stableCandidateID(value.CandidateHash)
	}
	if err := s.Database.PutCandidate(ctx, value); err != nil {
		return model.UpdateCandidate{}, err
	}
	return value, nil
}

func retryableCandidateState(status model.CandidateStatus) bool {
	switch status {
	case model.CandidateStaging, model.CandidateVerified, model.CandidateMerging, model.CandidateValidating:
		return true
	default:
		return false
	}
}

func (s *Service) transition(ctx context.Context, candidate *model.UpdateCandidate, to model.CandidateStatus, code, summary string) error {
	if candidate.Status == to {
		return nil
	}
	if err := s.Database.TransitionCandidate(ctx, candidate.ID, to, code, summary); err != nil {
		return err
	}
	candidate.Status = to
	candidate.FailureCode = code
	candidate.FailureSummary = summary
	candidate.UpdatedAt = s.now()
	return nil
}

func (s *Service) recordTree(ctx context.Context, hash string, manifest objectstore.TreeManifest) error {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	now := s.now()
	return s.Database.PutObjectRecord(ctx, model.ObjectRecord{
		Hash: hash, Kind: model.ObjectTree, Size: int64(len(encoded)), RelativePath: hash,
		VerifiedAt: now, CreatedAt: now,
	})
}

func (s *Service) prepareReviewBundle(ctx context.Context, candidate model.UpdateCandidate, risks []string, details any) (model.ReviewBundle, error) {
	risks = append([]string(nil), risks...)
	sort.Strings(risks)
	evidence, err := s.buildReviewEvidence(candidate)
	if err != nil {
		return model.ReviewBundle{}, err
	}
	payload := struct {
		Version       int            `json:"version"`
		CandidateID   string         `json:"candidate_id"`
		CandidateHash string         `json:"candidate_hash"`
		ResolvedRef   string         `json:"resolved_ref"`
		RiskTypes     []string       `json:"risk_types"`
		Details       any            `json:"details,omitempty"`
		Evidence      reviewEvidence `json:"evidence"`
		CreatedAt     time.Time      `json:"created_at"`
	}{1, candidate.ID, candidate.CandidateHash, candidate.ResolvedRef, risks, details, evidence, s.now()}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return model.ReviewBundle{}, err
	}
	if len(encoded) > MaxReviewBundleBytes {
		return model.ReviewBundle{}, errors.New("lifecycle: review bundle exceeds bounded evidence size")
	}
	hash, size, err := s.Objects.PutBlob(ctx, strings.NewReader(string(encoded)))
	if err != nil {
		return model.ReviewBundle{}, err
	}
	now := s.now()
	if err := s.Database.PutObjectRecord(ctx, model.ObjectRecord{
		Hash: hash, Kind: model.ObjectBundle, Size: size, RelativePath: hash,
		VerifiedAt: now, CreatedAt: now,
	}); err != nil {
		return model.ReviewBundle{}, err
	}
	id, err := model.NewID("bundle")
	if err != nil {
		return model.ReviewBundle{}, err
	}
	riskJSON, _ := json.Marshal(risks)
	return model.ReviewBundle{
		ID: id, CandidateID: candidate.ID, CandidateHash: candidate.CandidateHash,
		ObjectHash: hash, RiskTypesJSON: string(riskJSON), CreatedAt: now,
	}, nil
}

func (s *Service) transitionNeedsReview(ctx context.Context, candidate *model.UpdateCandidate, code string, risks []string, details any) error {
	bundle, err := s.prepareReviewBundle(ctx, *candidate, risks, details)
	if err != nil {
		return err
	}
	if err := s.Database.TransitionCandidateWithReviewBundle(ctx, candidate.ID, code, "", bundle); err != nil {
		return err
	}
	candidate.Status = model.CandidateNeedsReview
	candidate.FailureCode = code
	candidate.FailureSummary = ""
	candidate.UpdatedAt = s.now()
	return nil
}

const (
	maxReviewChanges     = 8
	maxReviewText        = 8 << 10
	MaxReviewBundleBytes = 512 << 10
)

type reviewEvidence struct {
	BaselineTree   string         `json:"baseline_tree,omitempty"`
	LocalTree      string         `json:"local_tree,omitempty"`
	UpstreamTree   string         `json:"upstream_tree"`
	MergedTree     string         `json:"merged_tree,omitempty"`
	Changes        []reviewChange `json:"changes"`
	OmittedChanges int            `json:"omitted_changes,omitempty"`
}

type reviewChange struct {
	Path     string          `json:"path"`
	Change   string          `json:"change"`
	Baseline *reviewSnapshot `json:"baseline,omitempty"`
	Local    *reviewSnapshot `json:"local,omitempty"`
	Upstream *reviewSnapshot `json:"upstream,omitempty"`
	Merged   *reviewSnapshot `json:"merged,omitempty"`
}

type reviewSnapshot struct {
	Kind       objectstore.EntryKind `json:"kind"`
	Mode       uint32                `json:"mode"`
	ObjectHash string                `json:"object_hash,omitempty"`
	Size       int64                 `json:"size,omitempty"`
	LinkTarget string                `json:"link_target,omitempty"`
	Text       string                `json:"text,omitempty"`
	Truncated  bool                  `json:"truncated,omitempty"`
}

func (s *Service) buildReviewEvidence(candidate model.UpdateCandidate) (reviewEvidence, error) {
	allowText := candidate.ReviewClass == model.ReviewSemanticSkillOnly
	result := reviewEvidence{UpstreamTree: candidate.UpstreamTreeHash, MergedTree: candidate.MergedTreeHash, Changes: []reviewChange{}}
	if candidate.UpstreamTreeHash == "" {
		return result, errors.New("lifecycle: review candidate has no upstream tree")
	}
	upstream, err := s.Objects.ReadTree(candidate.UpstreamTreeHash)
	if err != nil {
		return result, err
	}
	var baseline, local, merged objectstore.TreeManifest
	if candidate.BaselineID != "" {
		value, err := s.Database.GetBaseline(context.Background(), candidate.BaselineID)
		if err != nil {
			return result, err
		}
		result.BaselineTree = value.TreeHash
		baseline, err = s.Objects.ReadTree(value.TreeHash)
		if err != nil {
			return result, err
		}
	}
	if candidate.OverlayID != "" {
		value, err := s.Database.GetOverlay(context.Background(), candidate.OverlayID)
		if err != nil {
			return result, err
		}
		result.LocalTree = value.TreeHash
		local, err = s.Objects.ReadTree(value.TreeHash)
		if err != nil {
			return result, err
		}
	}
	if candidate.MergedTreeHash != "" {
		merged, err = s.Objects.ReadTree(candidate.MergedTreeHash)
		if err != nil {
			return result, err
		}
	}
	maps := []map[string]objectstore.TreeEntry{
		manifestEntries(baseline), manifestEntries(local), manifestEntries(upstream), manifestEntries(merged),
	}
	paths := map[string]struct{}{}
	for _, entries := range maps {
		for path, entry := range entries {
			if entry.Kind != objectstore.EntryDir {
				paths[path] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		base, baseOK := maps[0][path]
		localEntry, localOK := maps[1][path]
		upstreamEntry, upstreamOK := maps[2][path]
		mergedEntry, mergedOK := maps[3][path]
		effective, effectiveOK := upstreamEntry, upstreamOK
		if candidate.MergedTreeHash != "" {
			effective, effectiveOK = mergedEntry, mergedOK
		}
		if baseOK == effectiveOK && (!baseOK || treeEntryEqual(base, effective)) &&
			(baseOK == localOK && (!baseOK || treeEntryEqual(base, localEntry))) {
			continue
		}
		if len(result.Changes) >= maxReviewChanges {
			result.OmittedChanges++
			continue
		}
		change := reviewChange{Path: path, Change: changeKind(baseOK, effectiveOK)}
		if baseOK {
			change.Baseline, err = s.reviewSnapshot(path, base, allowText)
			if err != nil {
				return result, err
			}
		}
		if localOK {
			change.Local, err = s.reviewSnapshot(path, localEntry, allowText)
			if err != nil {
				return result, err
			}
		}
		if upstreamOK {
			change.Upstream, err = s.reviewSnapshot(path, upstreamEntry, allowText)
			if err != nil {
				return result, err
			}
		}
		if mergedOK {
			change.Merged, err = s.reviewSnapshot(path, mergedEntry, allowText)
			if err != nil {
				return result, err
			}
		}
		result.Changes = append(result.Changes, change)
	}
	return result, nil
}

func manifestEntries(manifest objectstore.TreeManifest) map[string]objectstore.TreeEntry {
	result := make(map[string]objectstore.TreeEntry, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		result[entry.Path] = entry
	}
	return result
}

func treeEntryEqual(left, right objectstore.TreeEntry) bool {
	return left.Kind == right.Kind && left.Mode == right.Mode && left.ObjectHash == right.ObjectHash && left.Size == right.Size && left.LinkTarget == right.LinkTarget
}

func changeKind(before, after bool) string {
	switch {
	case !before && after:
		return "added"
	case before && !after:
		return "removed"
	default:
		return "modified"
	}
}

func (s *Service) reviewSnapshot(path string, entry objectstore.TreeEntry, allowText bool) (*reviewSnapshot, error) {
	result := &reviewSnapshot{Kind: entry.Kind, Mode: entry.Mode, ObjectHash: entry.ObjectHash, Size: entry.Size, LinkTarget: entry.LinkTarget}
	if entry.Kind != objectstore.EntryFile || !allowText || !reviewTextPath(path) {
		return result, nil
	}
	reader, err := s.Objects.OpenBlob(entry.ObjectHash)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxReviewText+1))
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) || !safeReviewText(data) {
		return result, nil
	}
	if len(data) > maxReviewText {
		const half = maxReviewText / 2
		data = append(append(append([]byte(nil), data[:half]...), []byte("\n... [ToolTend review excerpt truncated] ...\n")...), data[len(data)-half:]...)
		result.Truncated = true
	}
	result.Text = string(data)
	return result, nil
}

var (
	secretAssignment = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?key|token|secret|password|authorization)\s*[:=]\s*["']?[^\s"']{8,}`)
	credentialToken  = regexp.MustCompile(`(?i)(AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
	encodedSecret    = regexp.MustCompile(`[A-Za-z0-9+/_=-]{64,}`)
	credentialURL    = regexp.MustCompile(`(?i)https?://[^\s/@:]+:[^\s/@]+@`)
)

func reviewTextPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".mdx", ".txt":
		return true
	default:
		return false
	}
}

func safeReviewText(data []byte) bool {
	if secretAssignment.Match(data) || credentialToken.Match(data) || encodedSecret.Match(data) {
		return false
	}
	// Credential-bearing URLs are not matched by every assignment pattern.
	return !credentialURL.Match(data)
}

func manifestToMergeTree(objects *objectstore.Store, manifest objectstore.TreeManifest) (merge.Tree, error) {
	result := make(merge.Tree)
	for _, entry := range manifest.Entries {
		switch entry.Kind {
		case objectstore.EntryDir:
			continue
		case objectstore.EntrySymlink:
			result[entry.Path] = merge.Entry{Kind: merge.KindSymlink, Mode: fs.FileMode(entry.Mode), Data: []byte(entry.LinkTarget)}
		case objectstore.EntryFile:
			reader, err := objects.OpenBlob(entry.ObjectHash)
			if err != nil {
				return nil, err
			}
			data, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				return nil, errors.Join(readErr, closeErr)
			}
			result[entry.Path] = merge.Entry{Kind: merge.KindRegular, Mode: fs.FileMode(entry.Mode), Data: data}
		default:
			return nil, fmt.Errorf("lifecycle: unsupported object kind %q", entry.Kind)
		}
	}
	return result, nil
}

func writeMergeTree(root string, tree merge.Tree) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	paths := make([]string, 0, len(tree))
	for name := range tree {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		if filepath.IsAbs(name) || filepath.Clean(filepath.FromSlash(name)) == ".." || strings.HasPrefix(filepath.Clean(filepath.FromSlash(name)), ".."+string(filepath.Separator)) {
			return fmt.Errorf("lifecycle: merge output escapes root: %s", name)
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		entry := tree[name]
		switch entry.Kind {
		case merge.KindRegular:
			if err := os.WriteFile(target, entry.Data, entry.Mode.Perm()); err != nil {
				return err
			}
		case merge.KindSymlink:
			return fmt.Errorf("lifecycle: merged symlink requires review: %s", name)
		default:
			return fmt.Errorf("lifecycle: unsupported merged entry: %s", name)
		}
	}
	return nil
}

func (s *Service) treeManifest(hash string) (objectstore.TreeManifest, error) {
	if hash == "" {
		return objectstore.TreeManifest{}, errors.New("lifecycle: tree hash is required")
	}
	return s.Objects.ReadTree(hash)
}

func (s *Service) bindingContentRoot(binding model.Binding, runtime bool) (string, error) {
	if binding.Managed && binding.ActiveGenerationID != "" {
		return activation.GenerationPath(s.activationRoot(binding.ID, runtime), binding.ActiveGenerationID)
	}
	info, err := os.Stat(binding.InstallPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("lifecycle: binding install path is not a directory")
	}
	return binding.InstallPath, nil
}

func (s *Service) activationRoot(bindingID string, runtime bool) string {
	if runtime {
		return filepath.Join(s.Paths.RuntimesDir, bindingID)
	}
	return filepath.Join(s.Paths.GenerationsDir, bindingID)
}

func validateBindingPathID(value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return errors.New("lifecycle: binding ID must be one safe path segment")
	}
	return nil
}

func isRuntime(component model.LogicalComponent, source model.Source) bool {
	if source.Kind != model.SourceNPM && source.Kind != model.SourcePyPI {
		return false
	}
	return component.Kind == model.ComponentCLI || component.Kind == model.ComponentStdioMCP
}

func policyAdapterFor(component model.LogicalComponent, source model.Source, binding model.Binding) policy.Adapter {
	switch source.Kind {
	case model.SourceGit:
		switch component.Kind {
		case model.ComponentSkill:
			return policy.AdapterGitSkill
		case model.ComponentHook:
			return policy.AdapterGitHook
		default:
			return policy.AdapterGitPlugin
		}
	case model.SourceNPM:
		if strings.Contains(binding.InstallMethod, "npx") {
			return policy.AdapterNPX
		}
		return policy.AdapterNPM
	case model.SourcePyPI:
		if strings.Contains(binding.InstallMethod, "uvx") {
			return policy.AdapterUVX
		}
		if strings.Contains(binding.InstallMethod, "uv") {
			return policy.AdapterUV
		}
		return policy.AdapterPipx
	case model.SourceHomebrew:
		return policy.AdapterHomebrew
	case model.SourceHTTP:
		return policy.AdapterHTTPMCP
	default:
		return policy.AdapterUnknown
	}
}

func policyMode(value model.ApplyMode) policy.Mode { return policy.Mode(value) }

func (s *Service) loadPolicy(ctx context.Context, bindingID string) (model.Policy, error) {
	value, err := s.Database.GetPolicy(ctx, bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		value = model.DefaultPolicy()
		value.BindingID = bindingID
		value.UpdatedAt = s.now()
		return value, nil
	}
	return value, err
}

func (s *Service) generation(ctx context.Context, bindingID, id string) (model.Generation, error) {
	return s.Database.GetGeneration(ctx, bindingID, id)
}

func (s *Service) generations(ctx context.Context, bindingID string) ([]model.Generation, error) {
	return s.Database.ListGenerations(ctx, bindingID)
}

func manifestRisks(component model.LogicalComponent, baseline, upstream objectstore.TreeManifest) []string {
	var risks []string
	base := make(map[string]objectstore.TreeEntry, len(baseline.Entries))
	for _, entry := range baseline.Entries {
		base[entry.Path] = entry
	}
	for _, entry := range upstream.Entries {
		previous, existed := base[entry.Path]
		if entry.Kind == objectstore.EntrySymlink {
			risks = append(risks, "symlink_change")
		}
		if entry.Kind == objectstore.EntryFile && entry.Mode&0o111 != 0 && (!existed || previous.Mode != entry.Mode || previous.ObjectHash != entry.ObjectHash) {
			risks = append(risks, "executable_change")
		}
		if existed && previous.Mode != entry.Mode {
			risks = append(risks, "permission_change")
		}
		delete(base, entry.Path)
	}
	for _, removed := range base {
		if removed.Kind == objectstore.EntrySymlink || removed.Mode&0o111 != 0 {
			risks = append(risks, "executable_change")
		}
	}
	if component.Kind == model.ComponentHook {
		risks = append(risks, "hook_content_change", "trust_change")
	}
	return uniqueStrings(risks)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Service) generationHealthy(binding model.Binding, component model.LogicalComponent, source model.Source, generation model.Generation) bool {
	runtimeComponent := isRuntime(component, source)
	path, err := activation.GenerationPath(s.activationRoot(binding.ID, runtimeComponent), generation.ID)
	if err != nil {
		return false
	}
	var hash string
	if runtimeComponent {
		hash, err = activation.HashRuntimeGeneration(path)
	} else {
		hash, err = activation.HashGeneration(path)
	}
	if err != nil || generation.IntegrityHash == "" || hash != generation.IntegrityHash {
		return false
	}
	if component.Kind == model.ComponentHook {
		rel := hookExecutable(binding.InstallMethod)
		if rel == "" {
			return false
		}
		info, statErr := os.Stat(filepath.Join(path, filepath.FromSlash(rel)))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return false
		}
	}
	return s.Objects.VerifyTree(generation.TreeHash) == nil
}

func hookExecutable(installMethod string) string {
	const prefix = "tooltend-hook:"
	if !strings.HasPrefix(installMethod, prefix) {
		return ""
	}
	value := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(installMethod, prefix)))
	if value == "" || value == "." || filepath.IsAbs(value) || value == ".." || strings.HasPrefix(value, "../") {
		return ""
	}
	return value
}
