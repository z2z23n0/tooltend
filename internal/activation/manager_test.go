package activation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestActivateSwitchesAndCommitsReceipt(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	manager := Manager{Root: root, Store: store, Now: func() time.Time { return now }}

	receipt, err := manager.Activate(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if current, err := Current(root); err != nil || current != intent.NewGeneration {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if receipt.IntentID != intent.ID || receipt.GenerationHash != intent.ExpectedGenerationHash || !receipt.ActivatedAt.Equal(now) {
		t.Fatalf("receipt: %+v", receipt)
	}
	if phase := store.phase(intent.ID); phase != PhaseCommitted {
		t.Fatalf("phase=%s", phase)
	}

	// Repeating the same request returns the already persisted receipt.
	second, err := manager.Activate(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt, second) || store.receiptCount() != 1 {
		t.Fatalf("first=%+v second=%+v count=%d", receipt, second, store.receiptCount())
	}
}

func TestActivateHealthFailureRestoresOldGeneration(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	calls := 0
	manager := Manager{
		Root:  root,
		Store: store,
		Health: func(context.Context, string) error {
			calls++
			if calls == 2 {
				return errors.New("unhealthy")
			}
			return nil
		},
	}
	if _, err := manager.Activate(context.Background(), intent); err == nil {
		t.Fatal("expected health failure")
	}
	if current, err := Current(root); err != nil || current != intent.OldGeneration {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if phase := store.phase(intent.ID); phase != PhaseRolledBack {
		t.Fatalf("phase=%s", phase)
	}
}

func TestRecoverCompletesPointerSwitchedIntent(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	intent.Phase = PhasePrepared
	if err := store.SaveIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := SwitchCurrent(root, intent.NewGeneration); err != nil {
		t.Fatal(err)
	}

	manager := Manager{Root: root, Store: store}
	results, err := manager.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != RecoveryCommitted || results[0].Receipt == nil || !results[0].Receipt.Recovered {
		t.Fatalf("results=%+v", results)
	}
	if store.receiptCount() != 1 || store.phase(intent.ID) != PhaseCommitted {
		t.Fatalf("phase=%s receipts=%d", store.phase(intent.ID), store.receiptCount())
	}
	results, err = manager.Recover(context.Background())
	if err != nil || len(results) != 0 || store.receiptCount() != 1 {
		t.Fatalf("second recovery results=%+v err=%v", results, err)
	}
}

func TestRecoverPreparedIntentWithOldPointerRollsBackJournal(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	intent.Phase = PhasePrepared
	if err := store.SaveIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	results, err := (&Manager{Root: root, Store: store}).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != RecoveryRolledBack {
		t.Fatalf("results=%+v", results)
	}
	if current, _ := Current(root); current != intent.OldGeneration {
		t.Fatalf("current=%q", current)
	}
}

func TestRecoverHashMismatchRestoresOldGeneration(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	intent.Phase = PhasePrepared
	if err := store.SaveIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := SwitchCurrent(root, intent.NewGeneration); err != nil {
		t.Fatal(err)
	}
	newPath, _ := GenerationPath(root, intent.NewGeneration)
	if err := os.WriteFile(filepath.Join(newPath, "tool"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := (&Manager{Root: root, Store: store}).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != RecoveryRolledBack {
		t.Fatalf("results=%+v", results)
	}
	if current, _ := Current(root); current != intent.OldGeneration {
		t.Fatalf("current=%q", current)
	}
}

func TestRollbackRestoreFailureKeepsIntentRecoverable(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	healthCalls := 0
	manager := Manager{
		Root:  root,
		Store: store,
		Health: func(context.Context, string) error {
			healthCalls++
			if healthCalls == 2 {
				oldPath, _ := GenerationPath(root, intent.OldGeneration)
				if err := os.RemoveAll(oldPath); err != nil {
					return err
				}
				return errors.New("new generation is unhealthy")
			}
			return nil
		},
	}

	if _, err := manager.Activate(context.Background(), intent); err == nil || !strings.Contains(err.Error(), "restore old generation") {
		t.Fatalf("activation error=%v", err)
	}
	if current, err := Current(root); err != nil || current != intent.NewGeneration {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if phase := store.phase(intent.ID); phase != PhasePointerSwitched {
		t.Fatalf("phase=%s", phase)
	}
	if store.receiptCount() != 0 {
		t.Fatalf("receipts=%d", store.receiptCount())
	}
}

func TestRecoverDoesNotTerminateAgainstDamagedOldGeneration(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	intent.Phase = PhasePrepared
	if err := store.SaveIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	oldPath, _ := GenerationPath(root, intent.OldGeneration)
	if err := os.WriteFile(filepath.Join(oldPath, "tool"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (&Manager{Root: root, Store: store}).Recover(context.Background()); err == nil || !strings.Contains(err.Error(), "old generation failed recovery validation") {
		t.Fatalf("recovery error=%v", err)
	}
	if phase := store.phase(intent.ID); phase != PhasePrepared {
		t.Fatalf("phase=%s", phase)
	}
}

func TestRecoverUnexpectedPointerRestoresOldGeneration(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	intent.Phase = PhasePrepared
	if err := store.SaveIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	writeGeneration(t, root, "third", "third")
	if err := SwitchCurrent(root, "third"); err != nil {
		t.Fatal(err)
	}

	results, err := (&Manager{Root: root, Store: store}).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != RecoveryRolledBack {
		t.Fatalf("results=%+v", results)
	}
	if current, _ := Current(root); current != intent.OldGeneration {
		t.Fatalf("current=%q", current)
	}
}

func TestFailureAfterPointerSwitchIsRecoverable(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	crash := errors.New("simulated crash")
	manager := Manager{
		Root:  root,
		Store: store,
		Failpoint: func(point Failpoint) error {
			if point == FailAfterPointerSwitch {
				return crash
			}
			return nil
		},
	}
	if _, err := manager.Activate(context.Background(), intent); !errors.Is(err, crash) {
		t.Fatalf("got %v", err)
	}
	if current, _ := Current(root); current != intent.NewGeneration {
		t.Fatalf("current=%q", current)
	}
	results, err := (&Manager{Root: root, Store: store}).Recover(context.Background())
	if err != nil || len(results) != 1 || results[0].Action != RecoveryCommitted {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}

func TestActivationRejectsOldGenerationMutationAfterJournal(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	manager := Manager{Root: root, Store: store, Failpoint: func(point Failpoint) error {
		if point == FailAfterJournal {
			oldPath, _ := GenerationPath(root, intent.OldGeneration)
			return os.WriteFile(filepath.Join(oldPath, "tool"), []byte("changed"), 0o644)
		}
		return nil
	}}
	if _, err := manager.Activate(context.Background(), intent); err == nil || !strings.Contains(err.Error(), "old generation hash changed") {
		t.Fatalf("activation error=%v", err)
	}
	if current, _ := Current(root); current != intent.OldGeneration {
		t.Fatalf("current=%q", current)
	}
}

func TestActivationRejectsNewGenerationMutationAfterPointerSwitch(t *testing.T) {
	root, intent := activationFixture(t)
	store := newMemoryStore()
	manager := Manager{Root: root, Store: store, Failpoint: func(point Failpoint) error {
		if point == FailAfterPointerSwitch {
			newPath, _ := GenerationPath(root, intent.NewGeneration)
			return os.WriteFile(filepath.Join(newPath, "tool"), []byte("changed"), 0o644)
		}
		return nil
	}}
	if _, err := manager.Activate(context.Background(), intent); err == nil || !strings.Contains(err.Error(), "new generation hash changed") {
		t.Fatalf("activation error=%v", err)
	}
	if current, _ := Current(root); current != intent.OldGeneration {
		t.Fatalf("current=%q", current)
	}
}

func TestActivationRejectsPointerReplacementBeforeCommit(t *testing.T) {
	root, intent := activationFixture(t)
	writeGeneration(t, root, "third", "third")
	store := newMemoryStore()
	manager := Manager{Root: root, Store: store, Failpoint: func(point Failpoint) error {
		if point == FailAfterPointerPersist {
			return SwitchCurrent(root, "third")
		}
		return nil
	}}
	if _, err := manager.Activate(context.Background(), intent); err == nil || !strings.Contains(err.Error(), "current generation changed") {
		t.Fatalf("activation error=%v", err)
	}
	if current, _ := Current(root); current != intent.OldGeneration {
		t.Fatalf("current=%q", current)
	}
}

func TestSwitchCurrentReadersOnlySeeWholePointers(t *testing.T) {
	root := t.TempDir()
	writeGeneration(t, root, "old", "old")
	writeGeneration(t, root, "new", "new")
	if err := SwitchCurrent(root, "old"); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsCh := make(chan error, 1)
	wait.Add(1)
	go func() {
		defer wait.Done()
		for i := 0; i < 500; i++ {
			current, err := Current(root)
			if err != nil || current != "old" && current != "new" {
				select {
				case errorsCh <- fmt.Errorf("reader observed incomplete pointer: current=%q err=%v", current, err):
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		generation := "old"
		if i%2 == 0 {
			generation = "new"
		}
		if err := SwitchCurrent(root, generation); err != nil {
			t.Fatal(err)
		}
	}
	wait.Wait()
	select {
	case err := <-errorsCh:
		t.Fatal(err)
	default:
	}
}

func TestHashGenerationRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("outside", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := HashGeneration(root); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func activationFixture(t *testing.T) (string, Intent) {
	t.Helper()
	root := t.TempDir()
	oldPath := writeGeneration(t, root, "old", "old")
	newPath := writeGeneration(t, root, "new", "new")
	if err := SwitchCurrent(root, "old"); err != nil {
		t.Fatal(err)
	}
	hash, err := HashGeneration(newPath)
	if err != nil {
		t.Fatal(err)
	}
	return root, Intent{
		ID:                        "activation-1",
		BindingID:                 "binding-1",
		CandidateID:               "candidate-1",
		CandidateHash:             strings.Repeat("c", 64),
		OldGeneration:             "old",
		NewGeneration:             "new",
		ExpectedGenerationHash:    hash,
		ExpectedOldGenerationHash: mustGenerationHash(t, oldPath),
	}
}

func mustGenerationHash(t *testing.T, path string) string {
	t.Helper()
	hash, err := HashGeneration(path)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func writeGeneration(t *testing.T, root, generation, content string) string {
	t.Helper()
	path, err := GenerationPath(root, generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "tool"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type storedIntent struct {
	intent Intent
	reason string
}

type memoryStore struct {
	mu       sync.Mutex
	intents  map[string]storedIntent
	receipts map[string]Receipt
}

func newMemoryStore() *memoryStore {
	return &memoryStore{intents: make(map[string]storedIntent), receipts: make(map[string]Receipt)}
}

func (s *memoryStore) SaveIntent(_ context.Context, intent Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.intents[intent.ID]; ok {
		left, right := current.intent, intent
		left.Phase, right.Phase = "", ""
		if !reflect.DeepEqual(left, right) {
			return errors.New("intent ID reused")
		}
		return nil
	}
	s.intents[intent.ID] = storedIntent{intent: intent}
	return nil
}

func (s *memoryStore) SetPhase(_ context.Context, id string, phase Phase, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.intents[id]
	if !ok {
		return errors.New("intent not found")
	}
	current.intent.Phase = phase
	current.reason = reason
	s.intents[id] = current
	return nil
}

func (s *memoryStore) Complete(_ context.Context, id string, receipt Receipt) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.receipts[id]; ok {
		return existing, nil
	}
	current, ok := s.intents[id]
	if !ok {
		return Receipt{}, errors.New("intent not found")
	}
	current.intent.Phase = PhaseCommitted
	s.intents[id] = current
	s.receipts[id] = receipt
	return receipt, nil
}

func (s *memoryStore) Pending(_ context.Context) ([]Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Intent
	for _, current := range s.intents {
		if current.intent.Phase == PhasePrepared || current.intent.Phase == PhasePointerSwitched {
			result = append(result, current.intent)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *memoryStore) phase(id string) Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intents[id].intent.Phase
}

func (s *memoryStore) receiptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.receipts)
}
