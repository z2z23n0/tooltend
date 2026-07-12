package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/store"
)

// Adopt moves a file-like binding behind a generation pointer, or rebuilds a
// package runtime at an exact version and creates a stable shim. Native package
// manager installations are never removed.
func (s *Service) Adopt(ctx context.Context, selector string, options AdoptOptions) (AdoptResult, error) {
	var result AdoptResult
	err := s.withMutationLock(ctx, func() error {
		var actionErr error
		result, actionErr = s.adopt(ctx, selector, options)
		return actionErr
	})
	return result, err
}

func (s *Service) adopt(ctx context.Context, selector string, options AdoptOptions) (AdoptResult, error) {
	component, bindings, err := s.Component(ctx, selector)
	if err != nil {
		return AdoptResult{}, err
	}
	binding, err := SelectBinding(bindings, options.BindingID)
	if err != nil {
		return AdoptResult{}, err
	}
	if err := validateBindingPathID(binding.ID); err != nil {
		return AdoptResult{}, err
	}
	if binding.Managed {
		return AdoptResult{}, errors.New("lifecycle: binding is already managed")
	}
	if binding.HostOwned() {
		return AdoptResult{}, fmt.Errorf("lifecycle: binding lifecycle is owned by %s", binding.LifecycleOwner())
	}
	resolvedSource, err := s.resolveAdoptSource(options)
	if err != nil {
		return AdoptResult{}, err
	}
	normalized, provider := resolvedSource.Source, resolvedSource.Provider
	if options.ExpectedSourceIdentity != "" && options.ExpectedSourceIdentity != resolvedSource.Identity {
		return AdoptResult{}, errors.New("lifecycle: source identity changed after confirmation preview")
	}
	now := s.now()
	sourceHash := resolvedSource.Identity
	sourceID := "src_" + sourceHash[:24]
	source := model.Source{
		ID: sourceID, Kind: model.SourceKind(normalized.Kind), Locator: normalized.Locator,
		Subdir: normalized.Subdir, PackageName: normalized.PackageName, IdentityHash: sourceHash,
		MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if component.SourceID != "" {
		existing, loadErr := s.Database.GetSource(ctx, component.SourceID)
		if loadErr != nil {
			return AdoptResult{}, loadErr
		}
		sameSource := existing.Kind == source.Kind && existing.Locator == source.Locator && existing.PackageName == source.PackageName && existing.Subdir == source.Subdir
		if !sameSource {
			// Inventory necessarily records a plain file installation as local (or
			// unknown) until the user supplies and verifies its authoritative
			// source. Rebinding that provisional identity is safe only when this
			// component has one binding; otherwise changing the component source
			// would silently affect sibling installations.
			provisional := existing.Kind == model.SourceLocal || existing.Kind == model.SourceUnknown
			if !provisional || len(bindings) != 1 {
				return AdoptResult{}, errors.New("lifecycle: component is already bound to a different source; source migration requires an isolated local or unknown binding")
			}
		} else {
			// Preserve the canonical source row, including its original creation
			// time and metadata. Explicit adoption may raise trust but must not
			// rewrite a source shared by sibling bindings.
			source = existing
			source.UpdatedAt = now
		}
	}
	runtimeComponent := isRuntime(component, source)
	if component.Kind == model.ComponentHook {
		return s.adoptHookBinding(ctx, component, source, binding, normalized, provider, options)
	}
	if runtimeComponent {
		return s.adoptRuntime(ctx, component, source, binding, normalized, provider, options)
	}
	return s.adoptFileBinding(ctx, component, source, binding, normalized, provider, options)
}

func (s *Service) adoptHookBinding(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, normalized adapter.Source, provider adapter.Adapter, options AdoptOptions) (result AdoptResult, err error) {
	expectedComponent := component
	expectedBinding := binding
	if source.Kind != model.SourceGit {
		return result, errors.New("lifecycle: managed hooks require a Git source in v1")
	}
	if binding.ConfigPath == "" || binding.ConfigPointer == "" {
		return result, errors.New("lifecycle: hook binding is missing its host config locator")
	}
	if strings.TrimSpace(options.Executable) == "" {
		return result, errors.New("lifecycle: hook adoption requires --executable relative to the source root")
	}
	if err := checkExpectedObservedFile(binding.ConfigPath, options.ExpectedConfigHash); err != nil {
		return result, err
	}
	track := adapter.Track{Channel: string(model.TrackStable)}
	if strings.TrimSpace(options.Version) != "" {
		track.Channel, track.Constraint = string(model.TrackExact), strings.TrimSpace(options.Version)
	}
	resolved, err := provider.Resolve(ctx, normalized, track)
	if err != nil {
		return result, fmt.Errorf("lifecycle: resolve hook source: %w", err)
	}
	if options.ExpectedResolvedRef != "" && resolved.Ref != options.ExpectedResolvedRef {
		return result, fmt.Errorf("lifecycle: resolved ref changed after confirmation: expected %q, got %q", options.ExpectedResolvedRef, resolved.Ref)
	}
	stageID, err := model.NewID("adopt")
	if err != nil {
		return result, err
	}
	stage := filepath.Join(s.Paths.StagingDir, binding.ID, stageID)
	artifact, err := provider.Fetch(ctx, normalized, resolved, stage)
	if err != nil {
		return result, fmt.Errorf("lifecycle: fetch hook source: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := provider.Verify(ctx, normalized, resolved, artifact); err != nil {
		return result, fmt.Errorf("lifecycle: verify hook source: %w", err)
	}
	executableRel, err := selectHookExecutable(artifact.Root, options.Executable)
	if err != nil {
		return result, err
	}
	treeHash, manifest, err := s.Objects.CaptureTree(ctx, artifact.Root, objectstore.CaptureOptions{})
	if err != nil {
		return result, err
	}
	if err := s.recordTree(ctx, treeHash, manifest); err != nil {
		return result, err
	}
	root := s.activationRoot(binding.ID, false)
	if err := ensureNoAdoptionPointer(root); err != nil {
		return result, err
	}
	generationID, err := model.NewID("gen")
	if err != nil {
		return result, err
	}
	generationPath, err := activation.GenerationPath(root, generationID)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(generationPath), 0o700); err != nil {
		return result, err
	}
	if err := s.Objects.MaterializeTree(ctx, treeHash, generationPath); err != nil {
		return result, err
	}
	committed := false
	journalPrepared := false
	defer func() {
		if !committed && !journalPrepared {
			err = errors.Join(err, os.RemoveAll(generationPath))
		}
	}()
	integrity, err := activation.HashGeneration(generationPath)
	if err != nil {
		return result, err
	}
	managedExecutable := filepath.Join(root, "current", filepath.FromSlash(executableRel))
	if err := checkExpectedObservedFile(binding.ConfigPath, options.ExpectedConfigHash); err != nil {
		return result, err
	}
	currentCommand, err := host.CommandAtPointer(binding.ConfigPath, binding.ConfigPointer)
	if err != nil {
		return result, err
	}
	managedCommand, err := host.RewriteHookCommand(currentCommand, managedExecutable)
	if err != nil {
		return result, err
	}
	mutation, err := host.PlanCommandMutation(ctx, host.CommandMutationOptions{
		Host: modelHostName(binding.Host), ConfigPath: binding.ConfigPath, Pointer: binding.ConfigPointer, Command: managedCommand,
	})
	if err != nil {
		return result, err
	}
	baselineHash, err := stableHash(struct {
		BindingID string
		TreeHash  string
	}{binding.ID, treeHash})
	if err != nil {
		return result, err
	}
	baseline := model.Baseline{
		ID: "base_" + baselineHash[:24], BindingID: binding.ID, SourceID: source.ID,
		ResolvedRef: resolved.Ref, TreeHash: treeHash, CreatedAt: s.now(),
	}
	generation := model.Generation{ID: generationID, BindingID: binding.ID, ResolvedRef: resolved.Ref, TreeHash: treeHash, IntegrityHash: integrity, State: model.GenerationOriginal, CreatedAt: s.now()}
	originalVersion := binding.ObservedVersion
	component.SourceID, component.UpdatedAt = source.ID, s.now()
	binding.Managed = true
	binding.InstallMethod = "tooltend-hook:" + filepath.ToSlash(executableRel)
	binding.Classification = model.ClassificationClean
	binding.ObservedHash, binding.ObservedVersion = treeHash, resolved.Version
	binding.ActiveGenerationID, binding.TrustHash, binding.LastSeenAt = generationID, mutation.AfterSHA256, s.now()
	receipt, err := s.newAdoptReceipt(binding, generationID, originalVersion, resolved.Ref, treeHash, "", managedExecutable)
	if err != nil {
		return result, err
	}
	commit := store.AdoptionCommit{
		Source: source, Trust: adoptionTrust(source.ID, s.now()),
		ExpectedComponent: expectedComponent, Component: component,
		ExpectedBinding: expectedBinding, Binding: binding,
		Generation: generation, Baseline: &baseline, Receipt: receipt,
	}
	intentID, err := model.NewID("adoption")
	if err != nil {
		return result, err
	}
	effectsJSON, err := marshalAdoptionEffects(adoptionEffects{Config: &adoptionConfigEffect{
		Path: binding.ConfigPath, Pointer: binding.ConfigPointer, BeforeHash: mutation.BeforeSHA256,
		AfterHash: mutation.AfterSHA256, ManagedTarget: managedExecutable, Changed: mutation.Changed,
	}})
	if err != nil {
		return result, err
	}
	plan := store.AdoptionJournalPlan{
		Version: adoptionPlanVersion, Kind: store.AdoptionHook, Root: root, GenerationPath: generationPath,
		GenerationHash: integrity, EffectsJSON: effectsJSON, Commit: commit,
	}
	if err := s.Database.PrepareAdoption(ctx, intentID, plan, s.now()); err != nil {
		return result, err
	}
	journalPrepared = true
	if err := s.failAdoption(AdoptionFailAfterJournal); err != nil {
		return result, err
	}
	defer func() {
		if !committed && journalPrepared {
			if _, recoverErr := recoverOneAdoption(ctx, s.Database, s.Paths, intentID); recoverErr != nil {
				err = errors.Join(err, recoverErr)
			}
		}
	}()
	if err := activation.SwitchCurrent(root, generationID); err != nil {
		return result, err
	}
	if err := s.failAdoption(AdoptionFailAfterCurrent); err != nil {
		return result, err
	}
	if mutation.Changed {
		if err := host.ApplyMutation(mutation); err != nil {
			return result, err
		}
	}
	if err := s.failAdoption(AdoptionFailAfterEndpoint); err != nil {
		return result, err
	}
	phase, err := recoverOneAdoption(ctx, s.Database, s.Paths, intentID)
	if err != nil {
		return result, err
	}
	if phase != store.AdoptionCommitted {
		return result, fmt.Errorf("lifecycle: adoption endpoint validation ended in %s", phase)
	}
	committed = true
	return AdoptResult{ComponentID: component.ID, BindingID: binding.ID, Generation: generationID, TreeHash: treeHash, Shim: managedExecutable, Baseline: true, Receipt: receipt}, nil
}

func selectHookExecutable(root, requested string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(requested)))
	if clean == "" || clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("lifecycle: hook executable must be a safe relative source path")
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	relative, err := filepath.Rel(root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("lifecycle: hook executable escapes source root")
	}
	info, err := os.Lstat(full)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("lifecycle: hook executable must be an executable regular file")
	}
	return filepath.ToSlash(relative), nil
}

func modelHostName(value model.HostKind) host.Name {
	if value == model.HostClaude {
		return host.Claude
	}
	return host.Codex
}

func (s *Service) adoptFileBinding(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, normalized adapter.Source, provider adapter.Adapter, options AdoptOptions) (result AdoptResult, err error) {
	expectedComponent := component
	expectedBinding := binding
	originalVersion := binding.ObservedVersion
	if info, statErr := os.Lstat(binding.InstallPath); statErr != nil {
		return result, statErr
	} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("lifecycle: file binding must be a real directory before adoption")
	}
	treeHash, manifest, err := s.Objects.CaptureTree(ctx, binding.InstallPath, objectstore.CaptureOptions{})
	if err != nil {
		return result, err
	}
	if err := s.recordTree(ctx, treeHash, manifest); err != nil {
		return result, err
	}
	if options.ExpectedObservedHash != "" && treeHash != options.ExpectedObservedHash {
		return result, errors.New("lifecycle: local binding content changed after confirmation preview")
	}

	resolved := adapter.Resolved{Ref: "adopted:" + treeHash}
	baselineKnown := false
	baselineTreeHash := ""
	if strings.TrimSpace(options.Version) != "" {
		resolved, err = provider.Resolve(ctx, normalized, adapter.Track{Channel: string(model.TrackExact), Constraint: strings.TrimSpace(options.Version)})
		if err != nil {
			return result, fmt.Errorf("lifecycle: resolve adoption baseline: %w", err)
		}
		if options.ExpectedResolvedRef != "" && resolved.Ref != options.ExpectedResolvedRef {
			return result, fmt.Errorf("lifecycle: resolved ref changed after confirmation: expected %q, got %q", options.ExpectedResolvedRef, resolved.Ref)
		}
		stageID, idErr := model.NewID("adopt")
		if idErr != nil {
			return result, idErr
		}
		stage := filepath.Join(s.Paths.StagingDir, binding.ID, stageID)
		artifact, fetchErr := provider.Fetch(ctx, normalized, resolved, stage)
		if fetchErr != nil {
			return result, fmt.Errorf("lifecycle: fetch adoption baseline: %w", fetchErr)
		}
		defer os.RemoveAll(stage)
		if verifyErr := provider.Verify(ctx, normalized, resolved, artifact); verifyErr != nil {
			return result, fmt.Errorf("lifecycle: verify adoption baseline: %w", verifyErr)
		}
		upstreamHash, upstreamManifest, captureErr := s.Objects.CaptureTree(ctx, artifact.Root, objectstore.CaptureOptions{})
		if captureErr != nil {
			return result, captureErr
		}
		if err := s.recordTree(ctx, upstreamHash, upstreamManifest); err != nil {
			return result, err
		}
		// The exact verified upstream is the Baseline even when the observed
		// binding has local edits. The local tree remains the original generation
		// and Update will derive its per-binding Overlay against this baseline.
		baselineKnown = true
		baselineTreeHash = upstreamHash
	}
	if options.ExpectedResolvedRef != "" && resolved.Ref != options.ExpectedResolvedRef {
		return result, fmt.Errorf("lifecycle: resolved ref changed after confirmation: expected %q, got %q", options.ExpectedResolvedRef, resolved.Ref)
	}

	root := s.activationRoot(binding.ID, false)
	if err := ensureNoAdoptionPointer(root); err != nil {
		return result, err
	}
	generationID, err := model.NewID("gen")
	if err != nil {
		return result, err
	}
	generationPath, err := activation.GenerationPath(root, generationID)
	if err != nil {
		return result, err
	}
	if err := os.MkdirAll(filepath.Dir(generationPath), 0o700); err != nil {
		return result, err
	}
	if err := s.Objects.MaterializeTree(ctx, treeHash, generationPath); err != nil {
		return result, err
	}
	committed := false
	journalPrepared := false
	defer func() {
		if !committed && !journalPrepared {
			err = errors.Join(err, os.RemoveAll(generationPath))
		}
	}()
	integrity, err := activation.HashGeneration(generationPath)
	if err != nil {
		return result, err
	}
	if options.ExpectedObservedHash != "" {
		currentHash, fingerprintErr := s.Objects.FingerprintTree(ctx, binding.InstallPath, objectstore.CaptureOptions{})
		if fingerprintErr != nil {
			return result, fingerprintErr
		}
		if currentHash != options.ExpectedObservedHash {
			return result, errors.New("lifecycle: local binding content changed during adoption")
		}
	}
	var baseline *model.Baseline
	var expectedPolicy, nextPolicy *model.Policy
	if baselineKnown {
		baselineID, idErr := model.NewID("base")
		if idErr != nil {
			return result, idErr
		}
		baseline = &model.Baseline{
			ID: baselineID, BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref,
			TreeHash: baselineTreeHash, CreatedAt: s.now(),
		}
	} else {
		expectedPolicy, nextPolicy, err = s.manualAdoptionPolicy(ctx, binding.ID)
		if err != nil {
			return result, err
		}
	}
	generation := model.Generation{
		ID: generationID, BindingID: binding.ID, ResolvedRef: resolved.Ref, TreeHash: treeHash,
		IntegrityHash: integrity, State: model.GenerationOriginal, CreatedAt: s.now(),
	}
	component.SourceID, component.UpdatedAt = source.ID, s.now()
	binding.Managed = true
	binding.InstallMethod = "tooltend-generation"
	binding.ObservedHash = treeHash
	binding.ObservedVersion = resolved.Version
	binding.ActiveGenerationID = generationID
	if baselineKnown && baselineTreeHash == treeHash {
		binding.Classification = model.ClassificationClean
	} else if baselineKnown {
		binding.Classification = model.ClassificationCustomized
	} else {
		binding.Classification = model.ClassificationDetached
	}
	binding.LastSeenAt = s.now()
	intentID, err := model.NewID("adoption")
	if err != nil {
		return result, err
	}
	installEffect := adoptionInstallEffect{
		Path: binding.InstallPath, BackupPath: adoptionBackupPath(binding.InstallPath, intentID),
		TempPath: adoptionInstallTempPath(binding.InstallPath, intentID), Target: filepath.Join(root, "current"),
		BeforeTreeHash: treeHash,
	}
	for _, path := range []string{installEffect.BackupPath, installEffect.TempPath} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			if statErr == nil {
				return result, fmt.Errorf("lifecycle: adoption recovery path already exists: %s", path)
			}
			return result, statErr
		}
	}
	backup := installEffect.BackupPath
	receipt, err := s.newAdoptReceipt(binding, generationID, originalVersion, resolved.Ref, treeHash, backup, "")
	if err != nil {
		return result, err
	}
	commit := store.AdoptionCommit{
		Source: source, Trust: adoptionTrust(source.ID, s.now()),
		ExpectedComponent: expectedComponent, Component: component,
		ExpectedBinding: expectedBinding, Binding: binding,
		Generation: generation, Baseline: baseline,
		ExpectedPolicy: expectedPolicy, Policy: nextPolicy, Receipt: receipt,
	}
	effectsJSON, err := marshalAdoptionEffects(adoptionEffects{Install: &installEffect})
	if err != nil {
		return result, err
	}
	plan := store.AdoptionJournalPlan{
		Version: adoptionPlanVersion, Kind: store.AdoptionFile, Root: root, GenerationPath: generationPath,
		GenerationHash: integrity, EffectsJSON: effectsJSON, Commit: commit,
	}
	if err := s.Database.PrepareAdoption(ctx, intentID, plan, s.now()); err != nil {
		return result, err
	}
	journalPrepared = true
	if err := s.failAdoption(AdoptionFailAfterJournal); err != nil {
		return result, err
	}
	defer func() {
		if !committed && journalPrepared {
			if _, recoverErr := recoverOneAdoption(ctx, s.Database, s.Paths, intentID); recoverErr != nil {
				err = errors.Join(err, recoverErr)
			}
		}
	}()
	if err := activation.SwitchCurrent(root, generationID); err != nil {
		return result, err
	}
	if err := s.failAdoption(AdoptionFailAfterCurrent); err != nil {
		return result, err
	}
	if err := applyInstallEffect(installEffect); err != nil {
		return result, err
	}
	if err := s.failAdoption(AdoptionFailAfterEndpoint); err != nil {
		return result, err
	}
	phase, err := recoverOneAdoption(ctx, s.Database, s.Paths, intentID)
	if err != nil {
		return result, err
	}
	if phase != store.AdoptionCommitted {
		return result, fmt.Errorf("lifecycle: adoption endpoint validation ended in %s", phase)
	}
	committed = true
	return AdoptResult{
		ComponentID: component.ID, BindingID: binding.ID, Generation: generationID,
		TreeHash: treeHash, Backup: backup, Baseline: baselineKnown, Receipt: receipt,
	}, nil
}

func (s *Service) adoptRuntime(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, normalized adapter.Source, provider adapter.Adapter, options AdoptOptions) (result AdoptResult, err error) {
	expectedComponent := component
	expectedBinding := binding
	originalVersion := binding.ObservedVersion
	if component.Kind == model.ComponentStdioMCP {
		if err := checkExpectedObservedFile(binding.ConfigPath, options.ExpectedConfigHash); err != nil {
			return AdoptResult{}, err
		}
	} else if options.ExpectedObservedHash != "" {
		if err := checkExpectedObservedFile(binding.InstallPath, options.ExpectedObservedHash); err != nil {
			return AdoptResult{}, err
		}
	}
	committed := false
	journalPrepared := false
	root := ""
	generationPath := ""
	defer func() {
		if !committed && !journalPrepared && generationPath != "" {
			err = errors.Join(err, os.RemoveAll(generationPath))
		}
	}()
	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = strings.TrimSpace(binding.ObservedVersion)
	}
	if version == "" {
		return AdoptResult{}, errors.New("lifecycle: runtime adoption requires an exact current version")
	}
	resolved, err := provider.Resolve(ctx, normalized, adapter.Track{Channel: string(model.TrackExact), Constraint: version})
	if err != nil {
		return AdoptResult{}, fmt.Errorf("lifecycle: resolve exact runtime: %w", err)
	}
	if resolved.Version == "" {
		return AdoptResult{}, errors.New("lifecycle: runtime adapter did not resolve an exact version")
	}
	if options.ExpectedResolvedRef != "" && resolved.Ref != options.ExpectedResolvedRef {
		return AdoptResult{}, fmt.Errorf("lifecycle: resolved ref changed after confirmation: expected %q, got %q", options.ExpectedResolvedRef, resolved.Ref)
	}
	stageID, err := model.NewID("adopt")
	if err != nil {
		return AdoptResult{}, err
	}
	stage := filepath.Join(s.Paths.StagingDir, binding.ID, stageID)
	artifact, err := provider.Fetch(ctx, normalized, resolved, stage)
	if err != nil {
		return AdoptResult{}, fmt.Errorf("lifecycle: rebuild exact runtime: %w", err)
	}
	defer os.RemoveAll(stage)
	if err := provider.Verify(ctx, normalized, resolved, artifact); err != nil {
		return AdoptResult{}, fmt.Errorf("lifecycle: verify exact runtime: %w", err)
	}
	executableRel, executableName, err := selectRuntimeExecutable(artifact, options.Executable, component.Name, source.PackageName)
	if err != nil {
		return AdoptResult{}, err
	}
	treeHash, manifest, err := s.Objects.CaptureTree(ctx, artifact.Root, runtimeCaptureOptions(true))
	if err != nil {
		return AdoptResult{}, err
	}
	if err := s.recordTree(ctx, treeHash, manifest); err != nil {
		return AdoptResult{}, err
	}
	root = s.activationRoot(binding.ID, true)
	if err := ensureNoAdoptionPointer(root); err != nil {
		return result, err
	}
	generationID, err := model.NewID("gen")
	if err != nil {
		return AdoptResult{}, err
	}
	generationPath, err = activation.GenerationPath(root, generationID)
	if err != nil {
		return AdoptResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(generationPath), 0o700); err != nil {
		return AdoptResult{}, err
	}
	if err := s.Objects.MaterializeTree(ctx, treeHash, generationPath); err != nil {
		return AdoptResult{}, err
	}
	integrity, err := activation.HashRuntimeGeneration(generationPath)
	if err != nil {
		return AdoptResult{}, err
	}
	shimPath, err := s.runtimeShimPath(component, binding, executableName)
	if err != nil {
		return result, err
	}
	if component.Kind == model.ComponentCLI {
		if err := ensureCLIShimPrecedence(shimPath, s.pathEnv()); err != nil {
			return result, err
		}
	}
	target := filepath.Join(root, "current", filepath.FromSlash(executableRel))
	shimPrefix := "#!/bin/sh\n"
	if source.Kind == model.SourcePyPI {
		shimPrefix += "export PYTHONDONTWRITEBYTECODE=1\n"
	}
	shim := []byte(shimPrefix + "exec " + shellQuote(target) + " \"$@\"\n")
	nativePath := ""
	var managedArgs []string
	if component.Kind == model.ComponentCLI && filepath.IsAbs(binding.InstallPath) {
		nativePath = binding.InstallPath
	}
	if component.Kind == model.ComponentStdioMCP {
		if binding.ConfigPath == "" || binding.ConfigPointer == "" {
			return AdoptResult{}, errors.New("lifecycle: stdio MCP binding is missing its host config locator")
		}
		currentSpec, commandErr := host.CommandSpecAtPointer(binding.ConfigPath, binding.ConfigPointer)
		if commandErr != nil {
			return AdoptResult{}, commandErr
		}
		managedArgs, commandErr = host.StripRuntimeCarrierArgs(
			currentSpec.Command, currentSpec.Args, string(source.Kind), source.PackageName, executableName,
		)
		if commandErr != nil {
			return AdoptResult{}, commandErr
		}
		if filepath.IsAbs(currentSpec.Command) {
			nativePath = filepath.Clean(currentSpec.Command)
		}
	}
	if component.Kind == model.ComponentStdioMCP {
		if err := checkExpectedObservedFile(binding.ConfigPath, options.ExpectedConfigHash); err != nil {
			return AdoptResult{}, err
		}
	} else if options.ExpectedObservedHash != "" {
		if err := checkExpectedObservedFile(binding.InstallPath, options.ExpectedObservedHash); err != nil {
			return AdoptResult{}, err
		}
	}
	baselineID, err := model.NewID("base")
	if err != nil {
		return result, err
	}
	baseline := model.Baseline{
		ID: baselineID, BindingID: binding.ID, SourceID: source.ID, ResolvedRef: resolved.Ref,
		TreeHash: treeHash, CreatedAt: s.now(),
	}
	intentID, err := model.NewID("adoption")
	if err != nil {
		return result, err
	}
	shimEffect, err := planShimEffect(shimPath, shim, nativePath, intentID)
	if err != nil {
		return result, err
	}
	var mutation host.FileMutation
	var configEffect *adoptionConfigEffect
	if component.Kind == model.ComponentStdioMCP {
		hostName := host.Codex
		if binding.Host == model.HostClaude {
			hostName = host.Claude
		}
		mutation, err = host.PlanCommandMutation(ctx, host.CommandMutationOptions{
			Host: hostName, ConfigPath: binding.ConfigPath, Pointer: binding.ConfigPointer, Command: shimPath,
			ReplaceArgs: true, Args: managedArgs,
		})
		if err != nil {
			return result, err
		}
		binding.TrustHash = mutation.AfterSHA256
		configEffect = &adoptionConfigEffect{
			Path: binding.ConfigPath, Pointer: binding.ConfigPointer, BeforeHash: mutation.BeforeSHA256,
			AfterHash: mutation.AfterSHA256, ManagedTarget: shimPath, Changed: mutation.Changed,
		}
	}

	generation := model.Generation{
		ID: generationID, BindingID: binding.ID, ResolvedRef: resolved.Ref, TreeHash: treeHash,
		IntegrityHash: integrity, State: model.GenerationOriginal, CreatedAt: s.now(),
	}
	component.SourceID, component.UpdatedAt = source.ID, s.now()
	binding.Managed = true
	binding.InstallMethod = "tooltend-runtime:" + filepath.ToSlash(executableRel)
	binding.Classification = model.ClassificationClean
	binding.ObservedHash = treeHash
	binding.ObservedVersion = resolved.Version
	binding.ActiveGenerationID = generationID
	binding.LastSeenAt = s.now()
	shimBackup := shimEffect.BackupPath
	receipt, err := s.newAdoptReceipt(binding, generationID, originalVersion, resolved.Ref, treeHash, shimBackup, shimPath)
	if err != nil {
		return result, err
	}
	commit := store.AdoptionCommit{
		Source: source, Trust: adoptionTrust(source.ID, s.now()),
		ExpectedComponent: expectedComponent, Component: component,
		ExpectedBinding: expectedBinding, Binding: binding,
		Generation: generation, Baseline: &baseline, Receipt: receipt,
	}
	effectsJSON, err := marshalAdoptionEffects(adoptionEffects{Shim: &shimEffect, Config: configEffect})
	if err != nil {
		return result, err
	}
	kind := store.AdoptionRuntimeCLI
	if component.Kind == model.ComponentStdioMCP {
		kind = store.AdoptionRuntimeStdio
	}
	plan := store.AdoptionJournalPlan{
		Version: adoptionPlanVersion, Kind: kind, Root: root, GenerationPath: generationPath,
		GenerationHash: integrity, Runtime: true, EffectsJSON: effectsJSON, Commit: commit,
	}
	if err := s.Database.PrepareAdoption(ctx, intentID, plan, s.now()); err != nil {
		return result, err
	}
	journalPrepared = true
	if err := s.failAdoption(AdoptionFailAfterJournal); err != nil {
		return result, err
	}
	defer func() {
		if !committed && journalPrepared {
			if _, recoverErr := recoverOneAdoption(ctx, s.Database, s.Paths, intentID); recoverErr != nil {
				err = errors.Join(err, recoverErr)
			}
		}
	}()
	if err := activation.SwitchCurrent(root, generationID); err != nil {
		return result, err
	}
	if err := s.failAdoption(AdoptionFailAfterCurrent); err != nil {
		return result, err
	}
	if err := applyShimEffect(shimEffect, shim); err != nil {
		return result, fmt.Errorf("lifecycle: create stable runtime shim: %w", err)
	}
	if configEffect != nil && mutation.Changed {
		if err := host.ApplyMutation(mutation); err != nil {
			return result, err
		}
	}
	if err := s.failAdoption(AdoptionFailAfterEndpoint); err != nil {
		return result, err
	}
	phase, err := recoverOneAdoption(ctx, s.Database, s.Paths, intentID)
	if err != nil {
		return result, err
	}
	if phase != store.AdoptionCommitted {
		return result, fmt.Errorf("lifecycle: adoption endpoint validation ended in %s", phase)
	}
	committed = true
	return AdoptResult{
		ComponentID: component.ID, BindingID: binding.ID, Generation: generationID,
		TreeHash: treeHash, Shim: shimPath, Backup: shimBackup, Baseline: true, Receipt: receipt,
	}, nil
}

func (s *Service) newAdoptReceipt(binding model.Binding, generationID, fromRef, resolvedRef, treeHash, backup, shim string) (model.Receipt, error) {
	id, err := model.NewID("receipt")
	if err != nil {
		return model.Receipt{}, err
	}
	summary, err := json.Marshal(map[string]string{
		"tree_hash": treeHash, "native_install": binding.InstallPath, "backup": backup, "shim": shim,
	})
	if err != nil {
		return model.Receipt{}, err
	}
	value := model.Receipt{
		ID: id, BindingID: binding.ID, Action: model.ReceiptAdopt, NewGenerationID: generationID,
		FromRef: fromRef, ToRef: resolvedRef, Status: model.ReceiptSucceeded,
		SummaryJSON: string(summary), CreatedAt: s.now(),
	}
	return value, nil
}

func adoptionTrust(sourceID string, now time.Time) model.SourceTrust {
	return model.SourceTrust{SourceID: sourceID, Level: model.TrustVerified, ApprovedBy: "explicit_adopt", ApprovedAt: now}
}

func ensureNoAdoptionPointer(root string) error {
	current, err := activation.Current(root)
	if errors.Is(err, activation.ErrNoCurrent) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("lifecycle: unmanaged binding has an existing activation pointer %q; run tooltend doctor", current)
}

func (s *Service) manualAdoptionPolicy(ctx context.Context, bindingID string) (*model.Policy, *model.Policy, error) {
	current, err := s.Database.GetPolicy(ctx, bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		next := model.DefaultPolicy()
		next.BindingID = bindingID
		next.ApplyMode, next.LocalCapMode, next.UpdatedAt = model.ApplyManual, model.ApplyManual, s.now()
		return nil, &next, nil
	}
	if err != nil {
		return nil, nil, err
	}
	next := current
	next.ApplyMode, next.LocalCapMode, next.UpdatedAt = model.ApplyManual, model.ApplyManual, s.now()
	return &current, &next, nil
}

func parseAdoptSource(value string) (adapter.Source, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return adapter.Source{}, errors.New("lifecycle: adoption source is required")
	}
	for prefix, kind := range map[string]adapter.SourceKind{
		"npm:": adapter.SourceNPM, "pypi:": adapter.SourcePyPI, "pipx:": adapter.SourcePyPI,
		"uv:": adapter.SourcePyPI, "uvx:": adapter.SourcePyPI, "brew:": adapter.SourceHomebrew,
	} {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			name := strings.TrimSpace(value[len(prefix):])
			return adapter.Source{Kind: kind, Locator: name, PackageName: name}, nil
		}
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "ssh://") || strings.HasPrefix(value, "git://") || strings.HasPrefix(value, "git@") {
		if strings.HasPrefix(value, "https://") && strings.Contains(value, "/mcp") && !strings.HasSuffix(value, ".git") {
			// Explicit HTTP MCP sources should use the http: prefix because a
			// repository URL is otherwise the safer interpretation.
			return adapter.Source{Kind: adapter.SourceGit, Locator: value}, nil
		}
		return adapter.Source{Kind: adapter.SourceGit, Locator: strings.TrimPrefix(value, "git+")}, nil
	}
	if strings.HasPrefix(strings.ToLower(value), "http:") {
		return adapter.Source{Kind: adapter.SourceHTTP, Locator: strings.TrimSpace(value[len("http:"):])}, nil
	}
	return adapter.Source{}, errors.New("lifecycle: source must be a Git URL or an npm:, pypi:, brew:, or http: coordinate")
}

func selectRuntimeExecutable(artifact adapter.Artifact, requested, componentName, packageName string) (string, string, error) {
	root, err := filepath.Abs(artifact.Root)
	if err != nil {
		return "", "", err
	}
	bin := artifact.Executable
	if bin == "" {
		return "", "", errors.New("lifecycle: adapter did not expose a runtime executable")
	}
	info, err := os.Stat(bin)
	if err != nil {
		return "", "", err
	}
	var candidates []string
	if info.IsDir() {
		for _, name := range uniqueStrings([]string{requested, componentName, filepath.Base(packageName)}) {
			if name == "" || filepath.Base(name) != name {
				continue
			}
			path := filepath.Join(bin, name)
			if item, statErr := os.Stat(path); statErr == nil && !item.IsDir() {
				candidates = append(candidates, path)
			}
		}
		if strings.TrimSpace(requested) != "" && len(candidates) == 0 {
			return "", "", fmt.Errorf("lifecycle: requested runtime executable %q was not found", requested)
		}
		if len(candidates) == 0 {
			entries, readErr := os.ReadDir(bin)
			if readErr != nil {
				return "", "", readErr
			}
			for _, entry := range entries {
				if item, statErr := os.Stat(filepath.Join(bin, entry.Name())); statErr == nil && !item.IsDir() {
					candidates = append(candidates, filepath.Join(bin, entry.Name()))
				}
			}
		}
	} else {
		candidates = append(candidates, bin)
	}
	sort.Strings(candidates)
	if len(candidates) != 1 {
		return "", "", errors.New("lifecycle: runtime executable is ambiguous; specify --executable")
	}
	rel, err := filepath.Rel(root, candidates[0])
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errors.New("lifecycle: runtime executable escapes staged runtime")
	}
	return filepath.ToSlash(rel), filepath.Base(candidates[0]), nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
