package activation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

type Manager struct {
	Root      string
	Store     Store
	Health    HealthCheck
	Hash      GenerationHash
	Now       func() time.Time
	Failpoint FailpointFunc
}

func (m *Manager) Activate(ctx context.Context, intent Intent) (Receipt, error) {
	if err := m.validateIntent(intent); err != nil {
		return Receipt{}, err
	}
	if err := m.fail(FailBeforeJournal); err != nil {
		return Receipt{}, err
	}

	actual, err := currentOrEmpty(m.Root)
	if err != nil {
		return Receipt{}, err
	}
	if actual != intent.OldGeneration && actual != intent.NewGeneration {
		return Receipt{}, fmt.Errorf("stale activation intent: current=%q old=%q", actual, intent.OldGeneration)
	}
	if intent.OldGeneration != "" {
		oldPath, pathErr := GenerationPath(m.Root, intent.OldGeneration)
		if pathErr != nil {
			return Receipt{}, pathErr
		}
		oldHash, hashErr := m.hash()(oldPath)
		if hashErr != nil {
			return Receipt{}, fmt.Errorf("old generation is not recoverable: %w", hashErr)
		}
		if intent.ExpectedOldGenerationHash == "" {
			intent.ExpectedOldGenerationHash = oldHash
		} else if oldHash != intent.ExpectedOldGenerationHash {
			return Receipt{}, errors.New("old generation hash changed before activation")
		}
	}

	newPath, err := GenerationPath(m.Root, intent.NewGeneration)
	if err != nil {
		return Receipt{}, err
	}
	actualHash, err := m.hash()(newPath)
	if err != nil {
		return Receipt{}, fmt.Errorf("hash new generation: %w", err)
	}
	if actualHash != intent.ExpectedGenerationHash {
		return Receipt{}, fmt.Errorf("new generation hash mismatch: got %s", actualHash)
	}
	if err := m.check(ctx, newPath); err != nil {
		return Receipt{}, fmt.Errorf("pre-activation health check: %w", err)
	}

	intent.Phase = PhasePrepared
	if err := m.Store.SaveIntent(ctx, intent); err != nil {
		return Receipt{}, fmt.Errorf("save activation intent: %w", err)
	}
	if err := m.fail(FailAfterJournal); err != nil {
		return Receipt{}, err
	}
	if err := m.verifyOld(intent); err != nil {
		_ = m.Store.SetPhase(ctx, intent.ID, PhaseRolledBack, "old generation changed after journal")
		return Receipt{}, err
	}

	if actual != intent.NewGeneration {
		if err := SwitchCurrent(m.Root, intent.NewGeneration); err != nil {
			rollbackErr := m.rollback(ctx, intent, "pointer switch failed")
			return Receipt{}, errors.Join(err, rollbackErr)
		}
	}
	if err := m.fail(FailAfterPointerSwitch); err != nil {
		return Receipt{}, err
	}
	if err := m.verifyCurrentNew(intent); err != nil {
		rollbackErr := m.rollback(ctx, intent, "pointer or generation changed after switch")
		return Receipt{}, errors.Join(err, rollbackErr)
	}
	if err := m.Store.SetPhase(ctx, intent.ID, PhasePointerSwitched, ""); err != nil {
		rollbackErr := m.restoreOld(intent)
		return Receipt{}, errors.Join(fmt.Errorf("persist pointer phase: %w", err), rollbackErr)
	}
	if err := m.fail(FailAfterPointerPersist); err != nil {
		return Receipt{}, err
	}

	if err := m.check(ctx, newPath); err != nil {
		rollbackErr := m.rollback(ctx, intent, "post-activation health check failed")
		return Receipt{}, errors.Join(fmt.Errorf("post-activation health check: %w", err), rollbackErr)
	}
	if err := m.verifyCurrentNew(intent); err != nil {
		rollbackErr := m.rollback(ctx, intent, "pointer or generation changed before commit")
		return Receipt{}, errors.Join(err, rollbackErr)
	}
	if err := m.verifyOld(intent); err != nil {
		rollbackErr := m.rollback(ctx, intent, "old generation changed before commit")
		return Receipt{}, errors.Join(err, rollbackErr)
	}

	receipt := m.receipt(intent, actualHash, false)
	receipt, err = m.Store.Complete(ctx, intent.ID, receipt)
	if err != nil {
		// The pointer and journal deliberately remain recoverable. Complete is
		// required to be transactional, so a retry can return the same receipt.
		return Receipt{}, fmt.Errorf("complete activation intent: %w", err)
	}
	if err := m.fail(FailAfterCommit); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// Recover reconciles every non-terminal journal entry against the real
// pointer. The pointer is authoritative only after its target hash and health
// checks succeed.
func (m *Manager) Recover(ctx context.Context) ([]RecoveryResult, error) {
	if m.Store == nil {
		return nil, errors.New("activation store is required")
	}
	intents, err := m.Store.Pending(ctx)
	if err != nil {
		return nil, fmt.Errorf("load activation intents: %w", err)
	}
	sort.Slice(intents, func(i, j int) bool { return intents[i].ID < intents[j].ID })
	results := make([]RecoveryResult, 0, len(intents))
	for _, intent := range intents {
		result, err := m.recoverOne(ctx, intent)
		if err != nil {
			return results, fmt.Errorf("recover intent %s: %w", intent.ID, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (m *Manager) recoverOne(ctx context.Context, intent Intent) (RecoveryResult, error) {
	if err := m.validateIntent(intent); err != nil {
		return RecoveryResult{}, err
	}
	actual, currentErr := currentOrEmpty(m.Root)
	if currentErr == nil && actual == intent.NewGeneration {
		newPath, err := GenerationPath(m.Root, intent.NewGeneration)
		if err == nil {
			var actualHash string
			actualHash, err = m.hash()(newPath)
			if err == nil && actualHash != intent.ExpectedGenerationHash {
				err = fmt.Errorf("generation hash mismatch")
			}
			if err == nil {
				if oldErr := m.verifyOld(intent); oldErr != nil {
					err = oldErr
				}
			}
			if err == nil {
				err = m.check(ctx, newPath)
			}
			if err == nil {
				if phaseErr := m.Store.SetPhase(ctx, intent.ID, PhasePointerSwitched, "recovered pointer"); phaseErr != nil {
					return RecoveryResult{}, phaseErr
				}
				receipt, completeErr := m.Store.Complete(ctx, intent.ID, m.receipt(intent, actualHash, true))
				if completeErr != nil {
					return RecoveryResult{}, completeErr
				}
				return RecoveryResult{IntentID: intent.ID, Action: RecoveryCommitted, Receipt: &receipt}, nil
			}
		}
		reason := "new generation failed recovery validation"
		if rollbackErr := m.rollback(ctx, intent, reason); rollbackErr != nil {
			return RecoveryResult{}, errors.Join(err, rollbackErr)
		}
		return RecoveryResult{IntentID: intent.ID, Action: RecoveryRolledBack, Reason: reason}, nil
	}

	if currentErr == nil && actual == intent.OldGeneration {
		if err := m.verifyOld(intent); err != nil {
			return RecoveryResult{}, fmt.Errorf("old generation failed recovery validation: %w", err)
		}
		if err := m.Store.SetPhase(ctx, intent.ID, PhaseRolledBack, "pointer remained old"); err != nil {
			return RecoveryResult{}, err
		}
		return RecoveryResult{IntentID: intent.ID, Action: RecoveryRolledBack, Reason: "pointer remained old"}, nil
	}

	reason := "unexpected or invalid current pointer"
	if err := m.rollback(ctx, intent, reason); err != nil {
		return RecoveryResult{}, errors.Join(currentErr, err)
	}
	return RecoveryResult{IntentID: intent.ID, Action: RecoveryRolledBack, Reason: reason}, nil
}

func (m *Manager) validateIntent(intent Intent) error {
	if m.Store == nil {
		return errors.New("activation store is required")
	}
	if intent.ID == "" || intent.BindingID == "" || intent.CandidateID == "" || intent.CandidateHash == "" {
		return errors.New("activation intent identity is incomplete")
	}
	if intent.ExpectedGenerationHash == "" {
		return errors.New("expected generation hash is required")
	}
	if m.Root == "" {
		return errors.New("activation root is required")
	}
	if _, err := GenerationPath(m.Root, intent.NewGeneration); err != nil {
		return err
	}
	if intent.OldGeneration != "" {
		if intent.OldGeneration == intent.NewGeneration {
			return errors.New("old and new generations must differ")
		}
		if _, err := GenerationPath(m.Root, intent.OldGeneration); err != nil {
			return err
		}
	}
	if err := validateCompletion(intent.Completion); err != nil {
		return err
	}
	return nil
}

func (m *Manager) rollback(ctx context.Context, intent Intent, reason string) error {
	if err := m.restoreOld(intent); err != nil {
		// The journal must stay non-terminal while the filesystem could not be
		// restored. A later recovery/doctor run can retry after the damaged or
		// missing old generation is repaired; marking it rolled back here would
		// permanently split SQLite state from the real pointer.
		return fmt.Errorf("restore old generation: %w", err)
	}
	return m.Store.SetPhase(ctx, intent.ID, PhaseRolledBack, reason)
}

func (m *Manager) restoreOld(intent Intent) error {
	if intent.OldGeneration == "" {
		return clearCurrent(m.Root)
	}
	if err := m.verifyOld(intent); err != nil {
		return err
	}
	return SwitchCurrent(m.Root, intent.OldGeneration)
}

func (m *Manager) hash() GenerationHash {
	if m.Hash != nil {
		return m.Hash
	}
	return HashGeneration
}

func (m *Manager) verifyCurrentNew(intent Intent) error {
	current, err := currentOrEmpty(m.Root)
	if err != nil {
		return err
	}
	if current != intent.NewGeneration {
		return fmt.Errorf("current generation changed during activation: got %q", current)
	}
	path, err := GenerationPath(m.Root, intent.NewGeneration)
	if err != nil {
		return err
	}
	hash, err := m.hash()(path)
	if err != nil {
		return err
	}
	if hash != intent.ExpectedGenerationHash {
		return errors.New("new generation hash changed during activation")
	}
	return nil
}

func (m *Manager) verifyOld(intent Intent) error {
	if intent.OldGeneration == "" {
		return nil
	}
	path, err := GenerationPath(m.Root, intent.OldGeneration)
	if err != nil {
		return err
	}
	hash, err := m.hash()(path)
	if err != nil {
		return err
	}
	if intent.ExpectedOldGenerationHash == "" || hash != intent.ExpectedOldGenerationHash {
		return errors.New("old generation hash changed during activation")
	}
	return nil
}

func validateCompletion(value Completion) error {
	if value.Action == "" {
		value.Action = CompletionUpdate
	}
	if value.Action != CompletionUpdate && value.Action != CompletionRollback {
		return fmt.Errorf("invalid activation completion action %q", value.Action)
	}
	if value.Baseline != nil {
		if value.Baseline.ID == "" || value.Baseline.SourceID == "" || value.Baseline.ResolvedRef == "" || value.Baseline.TreeHash == "" {
			return errors.New("activation baseline completion is incomplete")
		}
	}
	if value.Action == CompletionRollback && (value.FromRef == "" || value.ToRef == "") {
		return errors.New("rollback completion refs are required")
	}
	return nil
}

func (m *Manager) check(ctx context.Context, generationPath string) error {
	if m.Health == nil {
		return nil
	}
	return m.Health(ctx, generationPath)
}

func (m *Manager) receipt(intent Intent, generationHash string, recovered bool) Receipt {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	return Receipt{
		ID:             intent.ID,
		IntentID:       intent.ID,
		BindingID:      intent.BindingID,
		CandidateID:    intent.CandidateID,
		CandidateHash:  intent.CandidateHash,
		OldGeneration:  intent.OldGeneration,
		NewGeneration:  intent.NewGeneration,
		GenerationHash: generationHash,
		Action:         normalizedCompletionAction(intent.Completion.Action),
		FromRef:        intent.Completion.FromRef,
		ToRef:          intent.Completion.ToRef,
		ActivatedAt:    now().UTC(),
		Recovered:      recovered,
	}
}

func normalizedCompletionAction(value CompletionAction) CompletionAction {
	if value == "" {
		return CompletionUpdate
	}
	return value
}

func (m *Manager) fail(point Failpoint) error {
	if m.Failpoint == nil {
		return nil
	}
	if err := m.Failpoint(point); err != nil {
		return fmt.Errorf("activation failpoint %s: %w", point, err)
	}
	return nil
}

func currentOrEmpty(root string) (string, error) {
	current, err := Current(root)
	if errors.Is(err, ErrNoCurrent) {
		return "", nil
	}
	return current, err
}
