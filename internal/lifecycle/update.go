package lifecycle

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/merge"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/policy"
)

// Update resolves and, when policy and explicit options permit it, stages and
// activates one binding. A manual background check deliberately stops after
// Resolve: it never calls Fetch and therefore cannot mutate a package cache or
// run an installer.
func (s *Service) Update(ctx context.Context, selector, bindingID string, options UpdateOptions) (UpdateResult, error) {
	var result UpdateResult
	err := s.withMutationLock(ctx, func() error {
		var actionErr error
		result, actionErr = s.update(ctx, selector, bindingID, options)
		return actionErr
	})
	return result, err
}

func (s *Service) update(ctx context.Context, selector, bindingID string, options UpdateOptions) (UpdateResult, error) {
	component, bindings, err := s.Component(ctx, selector)
	if err != nil {
		return UpdateResult{}, err
	}
	binding, err := SelectBinding(bindings, bindingID)
	if err != nil {
		return UpdateResult{}, err
	}
	if err := validateBindingPathID(binding.ID); err != nil {
		return UpdateResult{}, err
	}
	if options.BindGeneration && binding.ActiveGenerationID != options.ExpectedGeneration {
		return UpdateResult{}, fmt.Errorf("lifecycle: active generation changed after confirmation: expected %q, got %q", options.ExpectedGeneration, binding.ActiveGenerationID)
	}
	result := UpdateResult{ComponentID: component.ID, BindingID: binding.ID}
	if component.SourceID == "" {
		return result, errors.New("lifecycle: component source is unknown")
	}
	source, err := s.Database.GetSource(ctx, component.SourceID)
	if err != nil {
		return result, err
	}
	updatePolicy, err := s.loadPolicy(ctx, binding.ID)
	if err != nil {
		return result, err
	}
	if updatePolicy.ApplyMode == model.ApplyIgnore || updatePolicy.LocalCapMode == model.ApplyIgnore {
		result.Decision = policy.Decide(policy.Input{LocalMode: policy.ModeIgnore})
		result.Warnings = []string{"binding is ignored by local policy"}
		return result, nil
	}
	provider, ok := s.Adapters.For(adapter.SourceKind(source.Kind))
	if !ok {
		return result, fmt.Errorf("lifecycle: no adapter for source %s", source.Kind)
	}
	normalized, err := provider.Normalize(sourceForAdapter(source))
	if err != nil {
		return result, err
	}
	capabilities := provider.Capabilities()
	if !capabilities.Check {
		result.Decision = policy.Decide(policy.Input{LocalMode: policyMode(updatePolicy.ApplyMode), Adapter: policyAdapterFor(component, source, binding)})
		result.Warnings = []string{"adapter cannot check this source safely"}
		return result, nil
	}
	resolved, err := provider.Resolve(ctx, normalized, trackForAdapter(updatePolicy))
	if err != nil {
		return result, fmt.Errorf("lifecycle: resolve update: %w", err)
	}
	if resolved.Ref == "" {
		return result, errors.New("lifecycle: adapter returned an empty resolved ref")
	}
	if options.ExpectedRef != "" && resolved.Ref != options.ExpectedRef {
		return result, fmt.Errorf("lifecycle: resolved ref changed after confirmation: expected %q, got %q", options.ExpectedRef, resolved.Ref)
	}
	result.ResolvedRef, result.Checked = resolved.Ref, true

	checkHash, err := stableHash(candidateIdentity{
		BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref,
		Rules: validationRulesVersion, Operation: "check",
	})
	if err != nil {
		return result, err
	}
	checkCandidate, err := s.putAvailableCandidate(ctx, model.UpdateCandidate{
		BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref, CandidateHash: checkHash,
	})
	if err != nil {
		return result, err
	}
	result.Candidate = checkCandidate

	baseline, baselineErr := s.Database.LatestBaseline(ctx, binding.ID)
	baselineKnown := baselineErr == nil && baseline.SourceID == source.ID && s.Objects.VerifyTree(baseline.TreeHash) == nil
	if baselineErr != nil && !errors.Is(baselineErr, sql.ErrNoRows) {
		return result, baselineErr
	}
	preliminary := s.policyDecision(component, source, binding, updatePolicy, resolved, baselineKnown, false, false, nil)
	result.Decision = preliminary
	alreadyCurrent := false
	if baselineKnown && baseline.ResolvedRef == resolved.Ref && binding.ActiveGenerationID != "" &&
		(updatePolicy.ExpectedIntegrity == "" || baseline.TreeHash == updatePolicy.ExpectedIntegrity) {
		if active, activeErr := s.generation(ctx, binding.ID, binding.ActiveGenerationID); activeErr == nil {
			alreadyCurrent = active.ResolvedRef == resolved.Ref && s.generationHealthy(binding, component, source, active)
		}
	}
	if alreadyCurrent {
		if checkCandidate.Status == model.CandidateAvailable {
			if err := s.transition(ctx, &checkCandidate, model.CandidateSuperseded, "already_current", ""); err != nil {
				return result, err
			}
		}
		result.Candidate = checkCandidate
		result.Warnings = append(result.Warnings, "resolved source already matches the active baseline")
		return result, nil
	}
	// Automatic work is only allowed for an already managed binding with a
	// verified baseline. Explicit update may stage a fork for human review, but
	// it still cannot activate it without the missing provenance.
	autoStage := updatePolicy.ApplyMode == model.ApplyAuto && updatePolicy.LocalCapMode == model.ApplyAuto && binding.Managed && baselineKnown
	explicitUpdate := options.Reason == "" || options.Reason == "manual_cli" || strings.HasPrefix(options.Reason, "manual_")
	wantStage := explicitUpdate && (options.Stage || options.Activate) || autoStage
	if !wantStage {
		if updatePolicy.ApplyMode == model.ApplyManual {
			result.Warnings = append(result.Warnings, "manual policy: update recorded without download")
		} else if !binding.Managed {
			result.Warnings = append(result.Warnings, "binding must be adopted before automatic staging")
		} else if !baselineKnown {
			result.Warnings = append(result.Warnings, "baseline is unknown; automatic staging is disabled")
		}
		return result, nil
	}
	if !capabilities.Stage {
		result.Warnings = append(result.Warnings, "adapter supports checks only; staging remains manual")
		return result, nil
	}

	stagingID, err := model.NewID("stage")
	if err != nil {
		return result, err
	}
	stagingPath := filepath.Join(s.Paths.StagingDir, binding.ID, stagingID)
	artifact, err := provider.Fetch(ctx, normalized, resolved, stagingPath)
	if err != nil {
		return result, fmt.Errorf("lifecycle: fetch update: %w", err)
	}
	defer os.RemoveAll(stagingPath)
	if err := provider.Verify(ctx, normalized, resolved, artifact); err != nil {
		return result, fmt.Errorf("lifecycle: verify update: %w", err)
	}
	upstreamHash, upstreamManifest, err := s.Objects.CaptureTree(ctx, artifact.Root, runtimeCaptureOptions(isRuntime(component, source)))
	if err != nil {
		return result, err
	}
	if updatePolicy.ExpectedIntegrity != "" && upstreamHash != updatePolicy.ExpectedIntegrity {
		return result, errors.New("lifecycle: resolved artifact does not match the project lock integrity")
	}
	if err := s.recordTree(ctx, upstreamHash, upstreamManifest); err != nil {
		return result, err
	}

	var overlay model.Overlay
	var localManifest objectstore.TreeManifest
	if baselineKnown {
		localRoot, rootErr := s.bindingContentRoot(binding, isRuntime(component, source))
		if rootErr != nil {
			baselineKnown = false
			result.Warnings = append(result.Warnings, "managed binding content is unavailable; treating it as a fork")
		} else {
			localHash, manifest, captureErr := s.Objects.CaptureTree(ctx, localRoot, runtimeCaptureOptions(isRuntime(component, source)))
			if captureErr != nil {
				return result, captureErr
			}
			if err := s.recordTree(ctx, localHash, manifest); err != nil {
				return result, err
			}
			if binding.Managed && binding.ActiveGenerationID != "" {
				if err := s.failSnapshot(SnapshotAfterObservedCapture); err != nil {
					return result, err
				}
				if err := s.freezeObservedGeneration(ctx, component, source, binding, localHash); err != nil {
					return result, err
				}
			}
			overlayID, idErr := model.NewID("overlay")
			if idErr != nil {
				return result, idErr
			}
			overlay = model.Overlay{ID: overlayID, BindingID: binding.ID, BaselineID: baseline.ID, TreeHash: localHash, Status: "clean", CreatedAt: s.now()}
			if localHash != baseline.TreeHash {
				overlay.Status = "customized"
			}
			if err := s.Database.PutOverlay(ctx, overlay); err != nil {
				return result, err
			}
			localManifest = manifest
		}
	}

	identity := candidateIdentity{
		BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref, Upstream: upstreamHash,
		Rules: validationRulesVersion, Operation: "update",
	}
	if baselineKnown {
		identity.Baseline = baseline.TreeHash
		// Bind review identity to content, never to the random database row ID.
		// Re-running the same stage therefore finds the reviewed candidate.
		identity.Overlay = overlay.TreeHash
	}
	candidateHash, err := stableHash(identity)
	if err != nil {
		return result, err
	}
	candidateValue := model.UpdateCandidate{
		BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref,
		UpstreamTreeHash: upstreamHash,
		CandidateHash:    candidateHash,
	}
	if baselineKnown {
		candidateValue.BaselineID, candidateValue.OverlayID = baseline.ID, overlay.ID
	}
	candidate, err := s.putAvailableCandidate(ctx, candidateValue)
	if err != nil {
		return result, err
	}
	result.Candidate = candidate
	if checkCandidate.ID != candidate.ID && checkCandidate.Status == model.CandidateAvailable {
		_ = s.Database.TransitionCandidate(ctx, checkCandidate.ID, model.CandidateSuperseded, "", "")
	}
	if candidate.Status != model.CandidateAvailable {
		return s.finishExistingCandidate(ctx, result, component, source, binding, updatePolicy, resolved, baselineKnown, options)
	}
	if err := s.transition(ctx, &candidate, model.CandidateStaging, "", ""); err != nil {
		return result, err
	}
	if err := s.transition(ctx, &candidate, model.CandidateVerified, "", ""); err != nil {
		return result, err
	}
	if err := s.transition(ctx, &candidate, model.CandidateMerging, "", ""); err != nil {
		return result, err
	}

	if !baselineKnown {
		candidate.ReviewClass = model.ReviewHumanRequired
		if err := s.Database.PutCandidate(ctx, candidate); err != nil {
			return result, err
		}
		if err := s.transitionNeedsReview(ctx, &candidate, "no_baseline", []string{"no_baseline"}, map[string]string{"merge_status": string(merge.StatusFork)}); err != nil {
			return result, err
		}
		result.Candidate, result.Staged, result.NeedsReview = candidate, true, true
		result.Decision = s.policyDecision(component, source, binding, updatePolicy, resolved, false, true, false, []string{"no_baseline"})
		result.Warnings = append(result.Warnings, "no verified baseline; candidate was not merged or activated")
		return result, nil
	}

	baselineManifest, err := s.treeManifest(baseline.TreeHash)
	if err != nil {
		return result, err
	}
	mergedHash := upstreamHash
	mergedManifest := upstreamManifest
	var mergeDetails any
	if isRuntime(component, source) {
		if overlay.TreeHash != baseline.TreeHash {
			mergeDetails = map[string]string{"reason": "runtime_customized"}
			candidate.ReviewClass = model.ReviewHumanRequired
			if err := s.Database.PutCandidate(ctx, candidate); err != nil {
				return result, err
			}
			if err := s.transitionNeedsReview(ctx, &candidate, "runtime_customized", []string{"runtime_customized"}, mergeDetails); err != nil {
				return result, err
			}
			result.Candidate, result.Staged, result.NeedsReview = candidate, true, true
			return result, nil
		}
	} else {
		baseTree, err := manifestToMergeTree(s.Objects, baselineManifest)
		if err != nil {
			return result, err
		}
		localTree, err := manifestToMergeTree(s.Objects, localManifest)
		if err != nil {
			return result, err
		}
		upstreamTree, err := manifestToMergeTree(s.Objects, upstreamManifest)
		if err != nil {
			return result, err
		}
		merged := merge.MergeTree(&baseTree, localTree, upstreamTree)
		mergeDetails = merged.Conflicts
		if merged.Status != merge.StatusMerged {
			candidate.ReviewClass = model.ReviewHumanRequired
			if err := s.Database.PutCandidate(ctx, candidate); err != nil {
				return result, err
			}
			if err := s.transitionNeedsReview(ctx, &candidate, "merge_conflict", []string{"merge_conflict"}, mergeDetails); err != nil {
				return result, err
			}
			result.Candidate, result.Staged, result.NeedsReview = candidate, true, true
			return result, nil
		}
		mergeDir, err := os.MkdirTemp(s.Paths.StagingDir, ".tooltend-merge-*")
		if err != nil {
			return result, err
		}
		defer os.RemoveAll(mergeDir)
		if err := writeMergeTree(mergeDir, merged.Tree); err != nil {
			return result, err
		}
		mergedHash, mergedManifest, err = s.Objects.CaptureTree(ctx, mergeDir, objectstore.CaptureOptions{})
		if err != nil {
			return result, err
		}
		if err := s.recordTree(ctx, mergedHash, mergedManifest); err != nil {
			return result, err
		}
	}
	candidate.MergedTreeHash = mergedHash
	if err := s.Database.PutCandidate(ctx, candidate); err != nil {
		return result, err
	}
	if err := s.transition(ctx, &candidate, model.CandidateValidating, "", ""); err != nil {
		return result, err
	}
	risks, validationErr := s.validateCandidateTree(component, isRuntime(component, source), baselineManifest, upstreamManifest, mergedManifest)
	if validationErr != nil {
		_ = s.transition(ctx, &candidate, model.CandidateFailed, "validation_failed", "")
		return result, validationErr
	}
	if len(risks) != 0 {
		candidate.ReviewClass = model.ReviewHumanRequired
		if component.Kind == model.ComponentSkill && onlySemanticRisk(risks) {
			candidate.ReviewClass = model.ReviewSemanticSkillOnly
		}
		if err := s.Database.PutCandidate(ctx, candidate); err != nil {
			return result, err
		}
		if err := s.transitionNeedsReview(ctx, &candidate, "review_required", risks, mergeDetails); err != nil {
			return result, err
		}
		result.NeedsReview = true
	} else if err := s.transition(ctx, &candidate, model.CandidateReady, "", ""); err != nil {
		return result, err
	}

	result.Candidate, result.Staged = candidate, true
	result.Decision = s.policyDecision(component, source, binding, updatePolicy, resolved, true, true, len(risks) == 0, risks)
	if candidate.Status != model.CandidateReady || !(options.Activate || autoStage) {
		return result, nil
	}
	// A manual command may request activation, but it receives no extra safety
	// authority. Re-evaluate the same evidence with auto policy and activate only
	// if the fail-closed policy engine finds no capability reason.
	safetyPolicy := updatePolicy
	safetyPolicy.ApplyMode, safetyPolicy.LocalCapMode = model.ApplyAuto, model.ApplyAuto
	safety := s.policyDecision(component, source, binding, safetyPolicy, resolved, true, true, true, nil)
	if safety.Mode != policy.ModeAuto {
		result.Warnings = append(result.Warnings, "candidate staged but activation safety evidence is incomplete")
		return result, nil
	}
	if err := s.activateUpdate(ctx, component, source, binding, updatePolicy, baseline, resolved, &candidate); err != nil {
		return result, err
	}
	result.Candidate, result.Activated = candidate, true
	return result, nil
}

func (s *Service) finishExistingCandidate(ctx context.Context, result UpdateResult, component model.LogicalComponent, source model.Source, binding model.Binding, updatePolicy model.Policy, resolved adapter.Resolved, baselineKnown bool, options UpdateOptions) (UpdateResult, error) {
	candidate := result.Candidate
	result.Staged = candidate.UpstreamTreeHash != ""
	result.NeedsReview = candidate.Status == model.CandidateNeedsReview
	result.Decision = s.policyDecision(component, source, binding, updatePolicy, resolved, baselineKnown, result.Staged, candidate.Status == model.CandidateReady || candidate.Status == model.CandidateActive, nil)
	if candidate.Status == model.CandidateActive {
		active, err := s.Database.GetGeneration(ctx, binding.ID, binding.ActiveGenerationID)
		if err != nil {
			return result, err
		}
		if active.CandidateID != candidate.ID || active.ResolvedRef != resolved.Ref {
			return result, errors.New("lifecycle: active candidate does not match the binding generation")
		}
		if err := s.ensureCandidateBaseline(ctx, binding, source, resolved, candidate); err != nil {
			return result, err
		}
		result.Activated = true
		return result, nil
	}
	autoActivate := updatePolicy.ApplyMode == model.ApplyAuto && updatePolicy.LocalCapMode == model.ApplyAuto && binding.Managed && baselineKnown
	if candidate.Status != model.CandidateReady || !(options.Activate || autoActivate) {
		result.Activated = candidate.Status == model.CandidateActive
		return result, nil
	}
	baseline, err := s.Database.LatestBaseline(ctx, binding.ID)
	if err != nil {
		return result, err
	}
	safetyPolicy := updatePolicy
	safetyPolicy.ApplyMode, safetyPolicy.LocalCapMode = model.ApplyAuto, model.ApplyAuto
	if s.policyDecision(component, source, binding, safetyPolicy, resolved, baselineKnown, true, true, nil).Mode != policy.ModeAuto {
		result.Warnings = append(result.Warnings, "candidate staged but activation safety evidence is incomplete")
		return result, nil
	}
	if err := s.activateUpdate(ctx, component, source, binding, updatePolicy, baseline, resolved, &candidate); err != nil {
		return result, err
	}
	result.Candidate, result.Activated = candidate, true
	return result, nil
}

func (s *Service) policyDecision(component model.LogicalComponent, source model.Source, binding model.Binding, value model.Policy, resolved adapter.Resolved, baselineKnown, staged, validated bool, risks []string) policy.Decision {
	runtimeComponent := isRuntime(component, source)
	rollbackVerified := false
	if binding.ActiveGenerationID != "" {
		if generation, err := s.generation(context.Background(), binding.ID, binding.ActiveGenerationID); err == nil {
			rollbackVerified = s.generationHealthy(binding, component, source, generation)
		}
	}
	stableShim := !runtimeComponent
	if runtimeComponent {
		name := runtimeExecutable(binding.InstallMethod)
		if name != "" {
			shimPath, pathErr := s.runtimeShimPath(component, binding, name)
			info, statErr := os.Lstat(shimPath)
			stableShim = pathErr == nil && statErr == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
		}
	}
	containsRisk := func(name string) bool {
		for _, value := range risks {
			if value == name {
				return true
			}
		}
		return false
	}
	return policy.Decide(policy.Input{
		Adapter: policyAdapterFor(component, source, binding), LocalMode: policyMode(value.ApplyMode),
		SourceKnown:   source.Kind != model.SourceUnknown && source.Kind != model.SourceLocal,
		BaselineKnown: baselineKnown, ManagedBinding: binding.Managed, ValidationPassed: staged && validated,
		RollbackVerified: rollbackVerified, Adopted: binding.Managed,
		RuntimeIsolated: !runtimeComponent || strings.HasPrefix(binding.InstallMethod, "tooltend-runtime:"),
		StableShim:      stableShim, ExactVersion: resolved.Version != "", PersistentRuntime: !runtimeComponent || binding.Managed,
		HookContentUnchanged: !containsRisk("hook_content_change"), TrustUnchanged: !containsRisk("trust_change"),
		PermissionsUnchanged: !containsRisk("permission_change") && !containsRisk("executable_change"),
		OldArtifactAvailable: rollbackVerified, DowngradeVerified: rollbackVerified,
	})
}

func (s *Service) validateCandidateTree(component model.LogicalComponent, runtimeComponent bool, baseline, upstream, merged objectstore.TreeManifest) ([]string, error) {
	if len(merged.Entries) == 0 {
		return nil, errors.New("lifecycle: candidate tree is empty")
	}
	// Exact isolated runtimes are immutable package-manager output. Executable
	// and symlink changes inside them are expected and stay behind the stable
	// shim; adapters already disable lifecycle scripts and verify the exact
	// installed package version.
	if runtimeComponent {
		return nil, nil
	}
	if component.Kind == model.ComponentSkill {
		found := false
		for _, entry := range merged.Entries {
			if entry.Path == "SKILL.md" && entry.Kind == objectstore.EntryFile {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("lifecycle: skill candidate is missing SKILL.md")
		}
	}
	risks := manifestRisks(component, baseline, upstream)
	if component.Kind == model.ComponentSkill && treeChanged(baseline, upstream) {
		risks = append(risks, "semantic_skill_change")
		eligible, secretLike := s.semanticSkillEvidenceEligible(baseline, merged)
		if !eligible {
			risks = append(risks, "non_semantic_skill_change")
		}
		if secretLike {
			risks = append(risks, "secret_like_content")
		}
	}
	if hasLifecycleScript(s.Objects, upstream) {
		risks = append(risks, "install_lifecycle_script")
	}
	return uniqueStrings(risks), nil
}

func (s *Service) semanticSkillEvidenceEligible(baseline, effective objectstore.TreeManifest) (eligible, secretLike bool) {
	eligible = true
	base, next := manifestEntries(baseline), manifestEntries(effective)
	paths := make(map[string]struct{}, len(base)+len(next))
	for path := range base {
		paths[path] = struct{}{}
	}
	for path := range next {
		paths[path] = struct{}{}
	}
	for path := range paths {
		before, beforeOK := base[path]
		after, afterOK := next[path]
		if beforeOK == afterOK && (!beforeOK || treeEntryEqual(before, after)) {
			continue
		}
		if !reviewTextPath(path) {
			eligible = false
		}
		if !afterOK {
			continue
		}
		if after.Kind != objectstore.EntryFile || after.Mode&0o111 != 0 || after.Size > maxReviewText {
			eligible = false
			continue
		}
		reader, err := s.Objects.OpenBlob(after.ObjectHash)
		if err != nil {
			return false, secretLike
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxReviewText+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || len(data) > maxReviewText || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			eligible = false
			continue
		}
		if !safeReviewText(data) {
			eligible, secretLike = false, true
		}
	}
	return eligible, secretLike
}

func treeChanged(left, right objectstore.TreeManifest) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) != string(rightJSON)
}

func hasLifecycleScript(objects *objectstore.Store, manifest objectstore.TreeManifest) bool {
	for _, entry := range manifest.Entries {
		if entry.Path != "package.json" || entry.Kind != objectstore.EntryFile {
			continue
		}
		reader, err := objects.OpenBlob(entry.ObjectHash)
		if err != nil {
			return true
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, 1<<20))
		_ = reader.Close()
		if readErr != nil {
			return true
		}
		var value struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &value) != nil {
			return true
		}
		for _, name := range []string{"preinstall", "install", "postinstall", "prepare"} {
			if strings.TrimSpace(value.Scripts[name]) != "" {
				return true
			}
		}
	}
	return false
}

func onlySemanticRisk(risks []string) bool {
	return len(risks) == 1 && risks[0] == "semantic_skill_change"
}

func (s *Service) activateUpdate(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, expectedPolicy model.Policy, oldBaseline model.Baseline, resolved adapter.Resolved, candidate *model.UpdateCandidate) error {
	oldGeneration, err := s.ensureCandidateFresh(ctx, component, source, binding, oldBaseline, *candidate)
	if err != nil {
		if candidate.Status == model.CandidateReady {
			_ = s.transition(ctx, candidate, model.CandidateSuperseded, "candidate_stale", "")
		}
		return err
	}
	if err := s.ensurePolicyFresh(ctx, expectedPolicy); err != nil {
		if candidate.Status == model.CandidateReady {
			_ = s.transition(ctx, candidate, model.CandidateSuperseded, "policy_changed", "")
		}
		return err
	}
	treeHash := candidate.MergedTreeHash
	if treeHash == "" {
		treeHash = candidate.UpstreamTreeHash
	}
	runtimeComponent := isRuntime(component, source)
	root := s.activationRoot(binding.ID, runtimeComponent)
	generationID, err := model.NewID("gen")
	if err != nil {
		return err
	}
	generationPath, err := activation.GenerationPath(root, generationID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(generationPath), 0o700); err != nil {
		return err
	}
	if err := s.Objects.MaterializeTree(ctx, treeHash, generationPath); err != nil {
		return err
	}
	var integrity string
	if runtimeComponent {
		integrity, err = activation.HashRuntimeGeneration(generationPath)
	} else {
		integrity, err = activation.HashGeneration(generationPath)
	}
	if err != nil {
		return err
	}
	generation := model.Generation{
		ID: generationID, BindingID: binding.ID, CandidateID: candidate.ID, ResolvedRef: resolved.Ref,
		TreeHash: treeHash, IntegrityHash: integrity, State: model.GenerationPrepared, CreatedAt: s.now(),
	}
	if err := s.Database.PutGeneration(ctx, generation); err != nil {
		return err
	}
	failPrepared := func(cause error) error {
		failed, cleanupErr := s.Database.FailPreparedGenerationIfUnjournaled(ctx, binding.ID, generationID)
		if failed {
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(generationPath))
		}
		return errors.Join(cause, cleanupErr)
	}
	journal, err := activation.NewSQLStore(s.Database)
	if err != nil {
		return failPrepared(err)
	}
	manager := activation.Manager{Root: root, Store: journal, Now: s.Now}
	if runtimeComponent {
		manager.Hash = activation.HashRuntimeGeneration
	}
	manager.Health = func(_ context.Context, path string) error {
		if component.Kind == model.ComponentSkill {
			if info, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
				return errors.New("skill health check failed")
			}
		}
		if runtimeComponent {
			rel := runtimeExecutable(binding.InstallMethod)
			if rel == "" {
				return errors.New("runtime executable is unknown")
			}
			if info, err := os.Stat(filepath.Join(path, filepath.FromSlash(rel))); err != nil || info.IsDir() {
				return errors.New("runtime executable health check failed")
			}
		}
		if component.Kind == model.ComponentHook {
			rel := hookExecutable(binding.InstallMethod)
			if rel == "" {
				return errors.New("hook executable is unknown")
			}
			if info, err := os.Stat(filepath.Join(path, filepath.FromSlash(rel))); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				return errors.New("hook executable health check failed")
			}
		}
		return nil
	}
	intentID, err := model.NewID("intent")
	if err != nil {
		return failPrepared(err)
	}
	if err := s.ensurePolicyFresh(ctx, expectedPolicy); err != nil {
		if candidate.Status == model.CandidateReady {
			_ = s.transition(ctx, candidate, model.CandidateSuperseded, "policy_changed", "")
		}
		return failPrepared(err)
	}
	baselineID := "base_" + candidate.CandidateHash[:24]
	_, err = manager.Activate(ctx, activation.Intent{
		ID: intentID, BindingID: binding.ID, CandidateID: candidate.ID, CandidateHash: candidate.CandidateHash,
		OldGeneration: binding.ActiveGenerationID, NewGeneration: generationID,
		ExpectedGenerationHash: integrity, ExpectedOldGenerationHash: oldGeneration.IntegrityHash,
		Completion: activation.Completion{
			Action: activation.CompletionUpdate, FromRef: oldGeneration.ResolvedRef, ToRef: resolved.Ref,
			Baseline: &activation.BaselineCompletion{
				ID: baselineID, SourceID: source.ID, ResolvedRef: resolved.Ref, TreeHash: candidate.UpstreamTreeHash,
			},
		},
	})
	if err != nil {
		return failPrepared(err)
	}
	candidate.Status = model.CandidateActive
	return nil
}

func (s *Service) ensureCandidateFresh(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, baseline model.Baseline, candidate model.UpdateCandidate) (model.Generation, error) {
	fresh, err := s.Database.GetBinding(ctx, binding.ID)
	if err != nil {
		return model.Generation{}, err
	}
	if !fresh.Managed || fresh.ActiveGenerationID != binding.ActiveGenerationID {
		return model.Generation{}, errors.New("lifecycle: candidate is stale because the active generation changed")
	}
	latest, err := s.Database.LatestBaseline(ctx, binding.ID)
	if err != nil {
		return model.Generation{}, err
	}
	if latest.ID != baseline.ID || latest.ID != candidate.BaselineID || latest.TreeHash != baseline.TreeHash || latest.SourceID != source.ID {
		return model.Generation{}, errors.New("lifecycle: candidate is stale because the baseline changed")
	}
	overlay, err := s.Database.GetOverlay(ctx, candidate.OverlayID)
	if err != nil {
		return model.Generation{}, err
	}
	root, err := s.bindingContentRoot(fresh, isRuntime(component, source))
	if err != nil {
		return model.Generation{}, err
	}
	currentHash, _, err := s.Objects.CaptureTree(ctx, root, runtimeCaptureOptions(isRuntime(component, source)))
	if err != nil {
		return model.Generation{}, err
	}
	if currentHash != overlay.TreeHash {
		return model.Generation{}, errors.New("lifecycle: candidate is stale because binding-local content changed after review")
	}
	if err := s.failSnapshot(SnapshotAfterCandidateCapture); err != nil {
		return model.Generation{}, err
	}
	currentGeneration, err := s.Database.GetGeneration(ctx, binding.ID, binding.ActiveGenerationID)
	if err != nil {
		return model.Generation{}, err
	}
	integrity, err := s.capturedTreeIntegrity(ctx, overlay.TreeHash, isRuntime(component, source))
	if err != nil {
		return model.Generation{}, err
	}
	if err := s.Database.CompareAndSetGenerationSnapshot(ctx, binding.ID, currentGeneration.ID,
		currentGeneration.TreeHash, currentGeneration.IntegrityHash, overlay.TreeHash, integrity); err != nil {
		return model.Generation{}, err
	}
	currentGeneration.TreeHash = overlay.TreeHash
	currentGeneration.IntegrityHash = integrity
	return currentGeneration, nil
}

func (s *Service) ensurePolicyFresh(ctx context.Context, expected model.Policy) error {
	current, err := s.Database.GetPolicy(ctx, expected.BindingID)
	if err != nil {
		return err
	}
	if current.BindingID != expected.BindingID || current.TrackChannel != expected.TrackChannel || current.Constraint != expected.Constraint ||
		current.ExpectedIntegrity != expected.ExpectedIntegrity || current.ApplyMode != expected.ApplyMode || current.NotifyMode != expected.NotifyMode ||
		current.LocalCapMode != expected.LocalCapMode || !current.UpdatedAt.Equal(expected.UpdatedAt) {
		return errors.New("lifecycle: policy changed before activation")
	}
	return nil
}

func (s *Service) freezeObservedGeneration(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, treeHash string) error {
	generation, err := s.Database.GetGeneration(ctx, binding.ID, binding.ActiveGenerationID)
	if err != nil {
		return err
	}
	integrity, err := s.capturedTreeIntegrity(ctx, treeHash, isRuntime(component, source))
	if err != nil {
		return err
	}
	return s.Database.CompareAndSetGenerationSnapshot(ctx, binding.ID, generation.ID,
		generation.TreeHash, generation.IntegrityHash, treeHash, integrity)
}

func (s *Service) ensureCandidateBaseline(ctx context.Context, binding model.Binding, source model.Source, resolved adapter.Resolved, candidate model.UpdateCandidate) error {
	if candidate.UpstreamTreeHash == "" {
		return errors.New("lifecycle: active candidate has no upstream baseline tree")
	}
	id := "base_" + candidate.CandidateHash[:24]
	return s.Database.PutBaseline(ctx, model.Baseline{
		ID: id, BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref,
		TreeHash: candidate.UpstreamTreeHash, CreatedAt: s.now(),
	})
}

func runtimeCaptureOptions(runtimeComponent bool) objectstore.CaptureOptions {
	if !runtimeComponent {
		return objectstore.CaptureOptions{}
	}
	return objectstore.CaptureOptions{Ignore: func(relative string, info os.FileInfo) bool {
		path := filepath.ToSlash(relative)
		return strings.Contains(path, "/__pycache__/") || strings.HasPrefix(path, "__pycache__/") ||
			strings.HasSuffix(path, ".pyc") || strings.HasSuffix(path, ".pyo") || strings.Contains(path, "/node_modules/.cache/")
	}}
}

func runtimeExecutable(installMethod string) string {
	const prefix = "tooltend-runtime:"
	if !strings.HasPrefix(installMethod, prefix) {
		return ""
	}
	return strings.TrimPrefix(installMethod, prefix)
}
