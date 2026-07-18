// Package reconcile runs ToolTend's daemonless, one-shot reconciliation loop.
package reconcile

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/z2z23n0/tooltend/internal/bundle"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/model"
)

const (
	ReasonScheduled = "scheduled"
	ReasonKick      = "kick"
	ReasonCommand   = "command"
)

type Request struct {
	Binding  model.Binding `json:"binding"`
	Policy   model.Policy  `json:"policy"`
	Stage    bool          `json:"stage"`
	Activate bool          `json:"activate"`
	Reason   string        `json:"reason"`
}

type Outcome struct {
	CandidateID   string `json:"candidate_id,omitempty"`
	CandidateHash string `json:"candidate_hash,omitempty"`
	ResolvedRef   string `json:"resolved_ref,omitempty"`
	Checked       bool   `json:"checked"`
	Changed       bool   `json:"changed"`
	Staged        bool   `json:"staged"`
	Activated     bool   `json:"activated"`
	NeedsReview   bool   `json:"needs_review"`
}

// Coordinator is the narrow seam between the generic worker and the
// lifecycle pipeline. Implementations resolve the source on every request;
// Stage and Activate are false for manual or unmanaged bindings.
type Coordinator interface {
	ReconcileBinding(context.Context, Request) (Outcome, error)
}

type CoordinatorFunc func(context.Context, Request) (Outcome, error)

func (f CoordinatorFunc) ReconcileBinding(ctx context.Context, request Request) (Outcome, error) {
	return f(ctx, request)
}

type RuntimeAdopter interface {
	AdoptRuntime(context.Context, model.Binding) (Outcome, error)
}

type RuntimeAdopterFunc func(context.Context, model.Binding) (Outcome, error)

func (f RuntimeAdopterFunc) AdoptRuntime(ctx context.Context, binding model.Binding) (Outcome, error) {
	return f(ctx, binding)
}

type BundleCoordinator interface {
	ReconcileBundle(context.Context, model.Bundle, model.BundlePolicy, bool) error
}

type BundleCoordinatorFunc func(context.Context, model.Bundle, model.BundlePolicy, bool) error

func (f BundleCoordinatorFunc) ReconcileBundle(ctx context.Context, value model.Bundle, policy model.BundlePolicy, activate bool) error {
	return f(ctx, value, policy, activate)
}

type BindingResult struct {
	BindingID string  `json:"binding_id"`
	TaskID    string  `json:"task_id"`
	Outcome   Outcome `json:"outcome"`
}

type FailureResult struct {
	BindingID string `json:"binding_id,omitempty"`
	BundleID  string `json:"bundle_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Code      string `json:"code"`
	Retrying  bool   `json:"retrying,omitempty"`
}

type RunResult struct {
	AlreadyRunning            bool                    `json:"already_running"`
	FailureNotificationQueued bool                    `json:"failure_notification_queued,omitempty"`
	RunID                     string                  `json:"run_id,omitempty"`
	ScanID                    string                  `json:"scan_id,omitempty"`
	StartedAt                 time.Time               `json:"started_at"`
	FinishedAt                time.Time               `json:"finished_at"`
	Recovered                 int                     `json:"recovered_activations"`
	BundleRecovery            bundle.RecoveryResult   `json:"bundle_recovery"`
	Inventory                 inventory.PersistResult `json:"inventory"`
	Scheduled                 int                     `json:"scheduled"`
	Succeeded                 int                     `json:"succeeded"`
	Retried                   int                     `json:"retried"`
	Failed                    int                     `json:"failed"`
	Skipped                   int                     `json:"skipped"`
	Results                   []BindingResult         `json:"results,omitempty"`
	Failures                  []FailureResult         `json:"failures,omitempty"`
}

// CodedError lets adapters expose a stable, non-sensitive reason code without
// allowing registry responses, paths, commands, or credentials into state.db.
type CodedError interface {
	error
	ReasonCode() string
	Retryable() bool
}

type codedError struct {
	code      string
	retryable bool
}

func (e codedError) Error() string      { return e.code }
func (e codedError) ReasonCode() string { return e.code }
func (e codedError) Retryable() bool    { return e.retryable }

var reasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func NewCodedError(code string, retryable bool) error {
	if !reasonCodePattern.MatchString(code) {
		return fmt.Errorf("reconcile: invalid reason code %q", code)
	}
	return codedError{code: code, retryable: retryable}
}
