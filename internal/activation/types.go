// Package activation manages crash-recoverable generation pointer switches.
package activation

import (
	"context"
	"errors"
	"time"
)

var ErrNoCurrent = errors.New("no active generation")

type Phase string

const (
	PhasePrepared        Phase = "prepared"
	PhasePointerSwitched Phase = "pointer_switched"
	PhaseCommitted       Phase = "committed"
	PhaseRolledBack      Phase = "rolled_back"
)

// Intent is the durable bridge between SQLite state and the filesystem
// pointer. OldGeneration must be retained until this intent is terminal.
type Intent struct {
	ID                        string     `json:"id"`
	BindingID                 string     `json:"binding_id"`
	CandidateID               string     `json:"candidate_id"`
	CandidateHash             string     `json:"candidate_hash"`
	OldGeneration             string     `json:"old_generation,omitempty"`
	NewGeneration             string     `json:"new_generation"`
	ExpectedGenerationHash    string     `json:"expected_generation_hash"`
	ExpectedOldGenerationHash string     `json:"expected_old_generation_hash,omitempty"`
	Completion                Completion `json:"completion,omitempty"`
	Phase                     Phase      `json:"phase"`
}

type CompletionAction string

const (
	CompletionUpdate   CompletionAction = "update"
	CompletionRollback CompletionAction = "rollback"
)

type BaselineCompletion struct {
	ID          string `json:"id"`
	SourceID    string `json:"source_id"`
	ResolvedRef string `json:"resolved_ref"`
	TreeHash    string `json:"tree_hash"`
}

// Completion is immutable metadata stored inside the activation journal so a
// recovered pointer switch can finish the exact same database transaction as
// the original process. It contains provenance only, never host configuration.
type Completion struct {
	Action                CompletionAction    `json:"action,omitempty"`
	FromRef               string              `json:"from_ref,omitempty"`
	ToRef                 string              `json:"to_ref,omitempty"`
	Baseline              *BaselineCompletion `json:"baseline,omitempty"`
	RolledBackCandidateID string              `json:"rolled_back_candidate_id,omitempty"`
}

type Receipt struct {
	ID             string           `json:"id"`
	IntentID       string           `json:"intent_id"`
	BindingID      string           `json:"binding_id"`
	CandidateID    string           `json:"candidate_id"`
	CandidateHash  string           `json:"candidate_hash"`
	OldGeneration  string           `json:"old_generation,omitempty"`
	NewGeneration  string           `json:"new_generation"`
	GenerationHash string           `json:"generation_hash"`
	Action         CompletionAction `json:"action"`
	FromRef        string           `json:"from_ref,omitempty"`
	ToRef          string           `json:"to_ref,omitempty"`
	ActivatedAt    time.Time        `json:"activated_at"`
	Recovered      bool             `json:"recovered,omitempty"`
}

// Store persists the intent journal. Complete must atomically mark the intent
// committed, update active state, and insert-or-return one receipt keyed by
// IntentID. SaveIntent must be idempotent for an identical intent and reject an
// ID reused with different fields.
type Store interface {
	SaveIntent(ctx context.Context, intent Intent) error
	SetPhase(ctx context.Context, intentID string, phase Phase, reason string) error
	Complete(ctx context.Context, intentID string, receipt Receipt) (Receipt, error)
	Pending(ctx context.Context) ([]Intent, error)
}

type HealthCheck func(ctx context.Context, generationPath string) error
type GenerationHash func(generationPath string) (string, error)

type Failpoint string

const (
	FailBeforeJournal       Failpoint = "before_journal"
	FailAfterJournal        Failpoint = "after_journal"
	FailAfterPointerSwitch  Failpoint = "after_pointer_switch"
	FailAfterPointerPersist Failpoint = "after_pointer_persist"
	FailAfterCommit         Failpoint = "after_commit"
)

// FailpointFunc is intended for deterministic crash-injection tests. A test
// may os.Exit from the callback to model a real process death.
type FailpointFunc func(point Failpoint) error

type RecoveryAction string

const (
	RecoveryCommitted  RecoveryAction = "committed"
	RecoveryRolledBack RecoveryAction = "rolled_back"
)

type RecoveryResult struct {
	IntentID string         `json:"intent_id"`
	Action   RecoveryAction `json:"action"`
	Reason   string         `json:"reason,omitempty"`
	Receipt  *Receipt       `json:"receipt,omitempty"`
}
