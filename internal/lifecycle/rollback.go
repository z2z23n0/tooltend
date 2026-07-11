package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/model"
)

// Rollback atomically points a managed binding at an older verified generation.
// The generation being left is retained, so a later rollback can move forward
// to it again. The pointer switch uses the same durable activation journal as a
// normal update.
func (s *Service) Rollback(ctx context.Context, selector, bindingID, to string) (RollbackResult, error) {
	return s.RollbackWithOptions(ctx, selector, bindingID, RollbackOptions{To: to})
}

// RollbackWithOptions binds an interactive confirmation to both the exact
// source generation and exact rollback target.
func (s *Service) RollbackWithOptions(ctx context.Context, selector, bindingID string, options RollbackOptions) (RollbackResult, error) {
	var result RollbackResult
	err := s.withMutationLock(ctx, func() error {
		var actionErr error
		result, actionErr = s.rollbackWithOptions(ctx, selector, bindingID, options)
		return actionErr
	})
	return result, err
}

func (s *Service) rollbackWithOptions(ctx context.Context, selector, bindingID string, options RollbackOptions) (RollbackResult, error) {
	component, bindings, err := s.Component(ctx, selector)
	if err != nil {
		return RollbackResult{}, err
	}
	binding, err := SelectBinding(bindings, bindingID)
	if err != nil {
		return RollbackResult{}, err
	}
	if err := validateBindingPathID(binding.ID); err != nil {
		return RollbackResult{}, err
	}
	if !binding.Managed || binding.ActiveGenerationID == "" {
		return RollbackResult{}, errors.New("lifecycle: only a managed active binding can be rolled back")
	}
	if options.ExpectedFromGeneration != "" && binding.ActiveGenerationID != options.ExpectedFromGeneration {
		return RollbackResult{}, fmt.Errorf("lifecycle: active generation changed after confirmation: expected %q, got %q", options.ExpectedFromGeneration, binding.ActiveGenerationID)
	}
	if component.SourceID == "" {
		return RollbackResult{}, errors.New("lifecycle: component source is unknown")
	}
	source, err := s.Database.GetSource(ctx, component.SourceID)
	if err != nil {
		return RollbackResult{}, err
	}
	values, err := s.generations(ctx, binding.ID)
	if err != nil {
		return RollbackResult{}, err
	}
	current, err := s.generation(ctx, binding.ID, binding.ActiveGenerationID)
	if err != nil {
		return RollbackResult{}, err
	}
	target, err := s.selectRollbackGeneration(ctx, binding, values, options.To)
	if err != nil {
		return RollbackResult{}, err
	}
	if !s.generationHealthy(binding, component, source, target) {
		return RollbackResult{}, errors.New("lifecycle: rollback target failed object or generation verification")
	}
	current, err = s.freezeRollbackSource(ctx, component, source, binding, current)
	if err != nil {
		return RollbackResult{}, err
	}

	baseline, _ := s.Database.LatestBaseline(ctx, binding.ID)
	hash, err := stableHash(candidateIdentity{
		BindingID: binding.ID, SourceID: source.ID, ResolvedRef: target.ResolvedRef,
		Upstream: target.TreeHash, Baseline: baseline.TreeHash, Overlay: current.TreeHash, Rules: validationRulesVersion,
		// The active generation's candidate changes each time the same pair is
		// traversed. Binding the attempt to it keeps crash retries idempotent but
		// lets gen2 -> gen1 run again after a completed round trip.
		Operation: "rollback:" + current.ID + ":" + target.ID + ":" + current.CandidateID,
	})
	if err != nil {
		return RollbackResult{}, err
	}
	candidate, err := s.putAvailableCandidate(ctx, model.UpdateCandidate{
		BindingID: binding.ID, SourceID: source.ID, ResolvedRef: target.ResolvedRef,
		UpstreamTreeHash: target.TreeHash, MergedTreeHash: target.TreeHash, BaselineID: baseline.ID,
		CandidateHash: hash,
	})
	if err != nil {
		return RollbackResult{}, err
	}
	if candidate.Status == model.CandidateAvailable {
		for _, status := range []model.CandidateStatus{
			model.CandidateStaging, model.CandidateVerified, model.CandidateMerging,
			model.CandidateValidating, model.CandidateReady,
		} {
			if err := s.transition(ctx, &candidate, status, "", ""); err != nil {
				return RollbackResult{}, err
			}
		}
	}
	if candidate.Status != model.CandidateReady {
		return RollbackResult{}, fmt.Errorf("lifecycle: rollback candidate is %s", candidate.Status)
	}

	runtimeComponent := isRuntime(component, source)
	root := s.activationRoot(binding.ID, runtimeComponent)
	journal, err := activation.NewSQLStore(s.Database)
	if err != nil {
		return RollbackResult{}, err
	}
	manager := activation.Manager{Root: root, Store: journal, Now: s.Now}
	if runtimeComponent {
		manager.Hash = activation.HashRuntimeGeneration
	}
	manager.Health = func(_ context.Context, root string) error {
		if component.Kind == model.ComponentSkill {
			if info, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
				return errors.New("rollback skill health check failed")
			}
		}
		if runtimeComponent {
			rel := runtimeExecutable(binding.InstallMethod)
			if rel == "" {
				return errors.New("rollback runtime executable is unknown")
			}
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil || info.IsDir() {
				return errors.New("rollback runtime executable health check failed")
			}
		}
		if component.Kind == model.ComponentHook {
			rel := hookExecutable(binding.InstallMethod)
			if rel == "" {
				return errors.New("rollback hook executable is unknown")
			}
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				return errors.New("rollback hook executable health check failed")
			}
		}
		return nil
	}
	intentID, err := model.NewID("intent")
	if err != nil {
		return RollbackResult{}, err
	}
	// Preserve provenance for the now-active exact target. For a customized
	// historical generation the source baseline remains its upstream tree,
	// while the generation itself remains the binding-local snapshot.
	baselineTree := target.TreeHash
	if target.CandidateID != "" {
		if historical, candidateErr := s.Database.GetCandidate(ctx, target.CandidateID); candidateErr == nil && historical.UpstreamTreeHash != "" {
			baselineTree = historical.UpstreamTreeHash
		}
	}
	baselineID, err := model.NewID("base")
	if err != nil {
		return RollbackResult{}, err
	}
	activationReceipt, err := manager.Activate(ctx, activation.Intent{
		ID: intentID, BindingID: binding.ID, CandidateID: candidate.ID, CandidateHash: candidate.CandidateHash,
		OldGeneration: current.ID, NewGeneration: target.ID,
		ExpectedGenerationHash: target.IntegrityHash, ExpectedOldGenerationHash: current.IntegrityHash,
		Completion: activation.Completion{
			Action: activation.CompletionRollback, FromRef: current.ResolvedRef, ToRef: target.ResolvedRef,
			Baseline: &activation.BaselineCompletion{
				ID: baselineID, SourceID: source.ID, ResolvedRef: target.ResolvedRef, TreeHash: baselineTree,
			},
			RolledBackCandidateID: current.CandidateID,
		},
	})
	if err != nil {
		return RollbackResult{}, err
	}
	summary, _ := json.Marshal(map[string]any{
		"generation_hash":   activationReceipt.GenerationHash,
		"activation_intent": intentID, "preserved_generation": current.ID,
	})
	receipt := model.Receipt{
		ID: intentID, BindingID: binding.ID, CandidateID: candidate.ID, Action: model.ReceiptRollback,
		OldGenerationID: current.ID, NewGenerationID: target.ID, FromRef: current.ResolvedRef, ToRef: target.ResolvedRef,
		CandidateHash: candidate.CandidateHash, Status: model.ReceiptSucceeded, SummaryJSON: string(summary), CreatedAt: s.now(),
	}
	return RollbackResult{
		ComponentID: component.ID, BindingID: binding.ID, From: current.ID, To: target.ID, Receipt: receipt,
	}, nil
}

func (s *Service) freezeRollbackSource(ctx context.Context, component model.LogicalComponent, source model.Source, binding model.Binding, generation model.Generation) (model.Generation, error) {
	root, err := s.bindingContentRoot(binding, isRuntime(component, source))
	if err != nil {
		return model.Generation{}, err
	}
	treeHash, manifest, err := s.Objects.CaptureTree(ctx, root, runtimeCaptureOptions(isRuntime(component, source)))
	if err != nil {
		return model.Generation{}, err
	}
	if err := s.recordTree(ctx, treeHash, manifest); err != nil {
		return model.Generation{}, err
	}
	if err := s.failSnapshot(SnapshotAfterRollbackCapture); err != nil {
		return model.Generation{}, err
	}
	integrity, err := s.capturedTreeIntegrity(ctx, treeHash, isRuntime(component, source))
	if err != nil {
		return model.Generation{}, err
	}
	if err := s.Database.CompareAndSetGenerationSnapshot(ctx, binding.ID, generation.ID,
		generation.TreeHash, generation.IntegrityHash, treeHash, integrity); err != nil {
		return model.Generation{}, err
	}
	generation.TreeHash = treeHash
	generation.IntegrityHash = integrity
	return generation, nil
}

// ResolveRollbackTarget returns the exact generations and refs that a later
// RollbackWithOptions call must preserve across confirmation.
func (s *Service) ResolveRollbackTarget(ctx context.Context, selector, bindingID, to string) (RollbackTarget, error) {
	component, bindings, err := s.Component(ctx, selector)
	if err != nil {
		return RollbackTarget{}, err
	}
	binding, err := SelectBinding(bindings, bindingID)
	if err != nil {
		return RollbackTarget{}, err
	}
	if err := validateBindingPathID(binding.ID); err != nil {
		return RollbackTarget{}, err
	}
	if !binding.Managed || binding.ActiveGenerationID == "" {
		return RollbackTarget{}, errors.New("lifecycle: only a managed active binding can be rolled back")
	}
	if component.SourceID == "" {
		return RollbackTarget{}, errors.New("lifecycle: component source is unknown")
	}
	source, err := s.Database.GetSource(ctx, component.SourceID)
	if err != nil {
		return RollbackTarget{}, err
	}
	values, err := s.generations(ctx, binding.ID)
	if err != nil {
		return RollbackTarget{}, err
	}
	current, err := s.generation(ctx, binding.ID, binding.ActiveGenerationID)
	if err != nil {
		return RollbackTarget{}, err
	}
	target, err := s.selectRollbackGeneration(ctx, binding, values, to)
	if err != nil {
		return RollbackTarget{}, err
	}
	if !s.generationHealthy(binding, component, source, target) {
		return RollbackTarget{}, errors.New("lifecycle: rollback target failed object or generation verification")
	}
	return RollbackTarget{
		ComponentID: component.ID, BindingID: binding.ID,
		FromGeneration: current.ID, FromRef: current.ResolvedRef,
		ToGeneration: target.ID, ToRef: target.ResolvedRef,
	}, nil
}

func (s *Service) selectRollbackGeneration(ctx context.Context, binding model.Binding, generations []model.Generation, to string) (model.Generation, error) {
	target := strings.TrimSpace(to)
	if target != "" {
		receipts, err := s.Database.ListReceipts(ctx, binding.ID, 1000)
		if err != nil {
			return model.Generation{}, err
		}
		for _, receipt := range receipts {
			if receipt.ID == target {
				target = receipt.NewGenerationID
				break
			}
		}
	}
	for _, generation := range generations {
		// A rollback target must have been active successfully before. Prepared,
		// original, active and failed rows are not historical rollback points;
		// only activation completion can transition a generation to inactive.
		if generation.ID == binding.ActiveGenerationID || generation.State != model.GenerationInactive {
			continue
		}
		if target == "" || generation.ID == target || generation.ResolvedRef == target ||
			strings.TrimPrefix(generation.ResolvedRef, "v") == strings.TrimPrefix(target, "v") ||
			strings.HasSuffix(generation.ResolvedRef, "@"+target) || strings.HasSuffix(generation.ResolvedRef, "=="+target) {
			return generation, nil
		}
	}
	if target == "" {
		return model.Generation{}, sql.ErrNoRows
	}
	return model.Generation{}, fmt.Errorf("lifecycle: rollback target %q was not found", to)
}
