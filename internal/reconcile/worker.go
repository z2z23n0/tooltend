package reconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/bundle"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/kick"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

const (
	defaultLease    = 5 * time.Minute
	defaultMaxTasks = 256
	maxAttempts     = 3
)

type InventoryOptions struct {
	Home           string
	CurrentProject string
	Projects       []string
}

type InventoryFunc func(context.Context, *store.Store, InventoryOptions) (inventory.PersistResult, error)
type BundleInventoryFunc func(context.Context, *store.Store) (bundle.DiscoverResult, error)
type BundleRecoveryFunc func(context.Context) (bundle.RecoveryResult, error)

type Worker struct {
	Database *store.Store
	Paths    config.Paths
	Config   config.Config
	HomeDir  string
	// CurrentProject is retained for construction compatibility. A background
	// reconciliation deliberately ignores process cwd and scans Config.Projects
	// only; init and explicit scan pass their working directory directly to the
	// inventory package.
	CurrentProject    string
	Coordinator       Coordinator
	RuntimeAdopter    RuntimeAdopter
	BundleCoordinator BundleCoordinator
	Inventory         InventoryFunc
	BundleInventory   BundleInventoryFunc
	BundleRecovery    BundleRecoveryFunc
	Recover           RecoveryFunc
	Now               func() time.Time
	Lease             time.Duration
	MaxTasks          int
}

func (w *Worker) RunOnce(ctx context.Context, reason string) (RunResult, error) {
	started := w.now()
	result := RunResult{StartedAt: started}
	finish := func() { result.FinishedAt = w.now() }
	if err := w.validate(); err != nil {
		finish()
		return result, err
	}
	lock, err := lockfile.Try(w.Paths.ActivationLock)
	if errors.Is(err, lockfile.ErrLocked) {
		result.AlreadyRunning = true
		finish()
		return result, nil
	}
	if err != nil {
		finish()
		return result, fmt.Errorf("reconcile: acquire activation lock: %w", err)
	}
	defer lock.Close()

	// The marker is only cleared by the worker that owns the global lock. A
	// concurrent process leaves it untouched for the active worker.
	if err := kick.Clear(w.Paths.StateDir); err != nil {
		finish()
		return result, fmt.Errorf("reconcile: clear kick marker: %w", err)
	}

	recoverFn := w.Recover
	if recoverFn == nil {
		recoverFn = RecoverActivations
	}
	result.Recovered, err = recoverFn(ctx, w.Database, w.Paths)
	if err != nil {
		finish()
		return result, fmt.Errorf("reconcile: activation recovery failed: %w", err)
	}
	if w.BundleRecovery != nil {
		result.BundleRecovery, err = w.BundleRecovery(ctx)
		if err != nil {
			finish()
			return result, fmt.Errorf("reconcile: bundle transaction recovery failed: %w", err)
		}
	}

	scanID, err := model.NewID("scan")
	if err != nil {
		finish()
		return result, err
	}
	result.ScanID = scanID
	reason = normalizeReason(reason)
	if err := w.Database.BeginScan(ctx, model.Scan{ID: scanID, Reason: reason, Status: "running", StartedAt: started}); err != nil {
		finish()
		return result, fmt.Errorf("reconcile: begin scan: %w", err)
	}

	inventoryFn := w.Inventory
	if inventoryFn == nil {
		inventoryFn = scanAndPersist
	}
	result.Inventory, err = inventoryFn(ctx, w.Database, InventoryOptions{
		Home: w.HomeDir, Projects: append([]string(nil), w.Config.Projects...),
	})
	if err != nil {
		_ = w.Database.FinishScan(ctx, scanID, "failed")
		finish()
		return result, fmt.Errorf("reconcile: inventory scan failed: %w", err)
	}
	if err := w.Database.FinishScan(ctx, scanID, "succeeded"); err != nil {
		finish()
		return result, fmt.Errorf("reconcile: finish scan: %w", err)
	}
	if w.BundleInventory != nil {
		if _, err := w.BundleInventory(ctx, w.Database); err != nil {
			finish()
			return result, fmt.Errorf("reconcile: bundle discovery failed: %w", err)
		}
	}

	signal, err := hookSignal(ctx, w.Database)
	if err != nil {
		finish()
		return result, fmt.Errorf("reconcile: read hook signal: %w", err)
	}
	bundleCounts, err := w.Database.BundleCounts(ctx)
	if err != nil {
		finish()
		return result, err
	}
	if bundleCounts.Total > 0 {
		if err := w.scheduleBundleTasks(ctx, signal, started, &result); err != nil {
			finish()
			return result, err
		}
		if err := markHookSignalsProcessed(ctx, w.Database, signal, started); err != nil {
			finish()
			return result, fmt.Errorf("reconcile: acknowledge hook signal: %w", err)
		}
		if err := w.runBundleTasks(ctx, &result); err != nil {
			finish()
			return result, err
		}
		finish()
		return result, nil
	}
	bindings, err := w.Database.ListBindings(ctx, "")
	if err != nil {
		finish()
		return result, err
	}
	for _, binding := range bindings {
		policy, policyErr := w.Database.GetPolicy(ctx, binding.ID)
		if policyErr != nil {
			if errors.Is(policyErr, sql.ErrNoRows) {
				result.Skipped++
				continue
			}
			finish()
			return result, policyErr
		}
		mode := effectiveMode(policy.ApplyMode, policy.LocalCapMode)
		if mode == model.ApplyIgnore {
			result.Skipped++
			continue
		}
		kind := "check"
		if mode == model.ApplyAuto && binding.Managed {
			kind = "update"
		}
		key := w.idempotencyKey(binding, policy, signal, started)
		task := model.Task{
			ID:             stableTaskID(key),
			Kind:           kind,
			BindingID:      binding.ID,
			IdempotencyKey: key,
			Status:         model.TaskPending,
			NextAttemptAt:  started,
			CreatedAt:      started,
			UpdatedAt:      started,
		}
		inserted, enqueueErr := w.Database.EnqueueTask(ctx, task)
		if enqueueErr != nil {
			finish()
			return result, enqueueErr
		}
		if inserted {
			result.Scheduled++
		}
	}
	if err := markHookSignalsProcessed(ctx, w.Database, signal, started); err != nil {
		finish()
		return result, fmt.Errorf("reconcile: acknowledge hook signal: %w", err)
	}

	if err := w.runTasks(ctx, reason, &result); err != nil {
		finish()
		return result, err
	}
	finish()
	return result, nil
}

func (w *Worker) scheduleBundleTasks(ctx context.Context, signal int64, started time.Time, result *RunResult) error {
	values, err := w.Database.ListBundles(ctx)
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.ConfigState != model.BundleConfigured {
			result.Skipped++
			continue
		}
		policy, err := w.Database.GetBundlePolicy(ctx, value.ID)
		if err != nil {
			return err
		}
		kind := ""
		switch policy.Mode {
		case model.BundlePolicyAuto:
			kind = "update"
		case model.BundlePolicyManual:
			kind = "check"
		case model.BundlePolicyObserve, model.BundlePolicyIgnore:
			result.Skipped++
			continue
		}
		key := w.bundleIdempotencyKey(value, policy, signal, started)
		task := model.BundleTask{ID: stableTaskID("bundle:" + key), BundleID: value.ID, Kind: kind, IdempotencyKey: "bundle:" + key,
			Status: model.TaskPending, NextAttemptAt: started, CreatedAt: started, UpdatedAt: started}
		inserted, err := w.Database.EnqueueBundleTask(ctx, task)
		if err != nil {
			return err
		}
		if inserted {
			result.Scheduled++
		}
	}
	return nil
}

func (w *Worker) runBundleTasks(ctx context.Context, result *RunResult) error {
	lease := w.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	limit := w.MaxTasks
	if limit <= 0 {
		limit = defaultMaxTasks
	}
	for processed := 0; processed < limit; processed++ {
		now := w.now()
		task, err := w.Database.ClaimBundleTask(ctx, now, lease)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		value, err := w.Database.GetBundle(ctx, task.BundleID)
		if err != nil {
			_ = w.Database.FailBundleTask(ctx, task.ID, "bundle_unavailable")
			result.Failed++
			continue
		}
		policy, err := w.Database.GetBundlePolicy(ctx, value.ID)
		if err != nil || value.ConfigState != model.BundleConfigured {
			_ = w.Database.FailBundleTask(ctx, task.ID, "bundle_policy_unavailable")
			result.Failed++
			continue
		}
		if w.BundleCoordinator == nil {
			_ = w.Database.FailBundleTask(ctx, task.ID, "bundle_coordinator_unavailable")
			result.Failed++
			continue
		}
		activate := task.Kind == "update" && policy.Mode == model.BundlePolicyAuto
		err = w.BundleCoordinator.ReconcileBundle(ctx, value, policy, activate)
		if err == nil {
			if err := w.Database.CompleteBundleTask(ctx, task.ID); err != nil {
				return err
			}
			result.Succeeded++
			continue
		}
		code, retryable := classifyError(err)
		if retryable && task.Attempts < maxAttempts {
			if err := w.Database.RetryBundleTask(ctx, task.ID, code, now.Add(retryDelay(task.Attempts))); err != nil {
				return err
			}
			result.Retried++
			continue
		}
		if err := w.Database.FailBundleTask(ctx, task.ID, code); err != nil {
			return err
		}
		result.Failed++
	}
	return nil
}

func (w *Worker) bundleIdempotencyKey(value model.Bundle, policy model.BundlePolicy, signal int64, now time.Time) string {
	interval := w.Config.Check.Interval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	slot := now.UnixNano() / interval.Nanoseconds()
	material := strings.Join([]string{"reconcile-bundle-v1", value.ID, value.CurrentReleaseID, string(policy.Mode),
		strconv.FormatInt(policy.UpdatedAt.UnixNano(), 10), strconv.FormatInt(slot, 10), strconv.FormatInt(signal, 10)}, "\x00")
	hash := sha256.Sum256([]byte(material))
	return hex.EncodeToString(hash[:])
}

func (w *Worker) runTasks(ctx context.Context, reason string, result *RunResult) error {
	lease := w.Lease
	if lease <= 0 {
		lease = defaultLease
	}
	limit := w.MaxTasks
	if limit <= 0 {
		limit = defaultMaxTasks
	}
	for processed := 0; processed < limit; processed++ {
		now := w.now()
		task, err := w.Database.ClaimTask(ctx, now, lease)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reconcile: claim task: %w", err)
		}
		binding, err := w.Database.GetBinding(ctx, task.BindingID)
		if err != nil {
			if failErr := w.Database.FailTask(ctx, task.ID, "binding_unavailable", ""); failErr != nil {
				return failErr
			}
			result.Failed++
			result.Failures = append(result.Failures, FailureResult{BindingID: task.BindingID, TaskID: task.ID, Code: "binding_unavailable"})
			continue
		}
		policy, err := w.Database.GetPolicy(ctx, binding.ID)
		if err != nil {
			if failErr := w.Database.FailTask(ctx, task.ID, "policy_unavailable", ""); failErr != nil {
				return failErr
			}
			result.Failed++
			result.Failures = append(result.Failures, FailureResult{BindingID: binding.ID, TaskID: task.ID, Code: "policy_unavailable"})
			continue
		}
		mode := effectiveMode(policy.ApplyMode, policy.LocalCapMode)
		if mode == model.ApplyIgnore {
			if err := w.Database.CompleteTask(ctx, task.ID); err != nil {
				return err
			}
			result.Skipped++
			continue
		}
		var outcome Outcome
		var coordinateErr error
		if task.Kind == "adopt_runtime_auto" {
			if binding.Managed {
				if err := w.Database.CompleteTask(ctx, task.ID); err != nil {
					return err
				}
				result.Skipped++
				continue
			}
			if w.RuntimeAdopter == nil {
				coordinateErr = NewCodedError("runtime_adopter_unavailable", false)
			} else {
				outcome, coordinateErr = w.RuntimeAdopter.AdoptRuntime(ctx, binding)
				if coordinateErr == nil && policy.ApplyMode == model.ApplyManual && policy.LocalCapMode == model.ApplyManual {
					next := policy
					next.ApplyMode, next.LocalCapMode, next.UpdatedAt = model.ApplyAuto, model.ApplyAuto, now
					coordinateErr = w.Database.CompareAndSetPolicy(ctx, policy, next)
				}
			}
		} else if task.Kind == "check" || task.Kind == "update" {
			auto := mode == model.ApplyAuto && binding.Managed
			outcome, coordinateErr = w.Coordinator.ReconcileBinding(ctx, Request{
				Binding: binding, Policy: policy, Stage: auto, Activate: auto, Reason: reason,
			})
		} else {
			coordinateErr = NewCodedError("unknown_task_kind", false)
		}
		if coordinateErr != nil {
			code, retryable := classifyError(coordinateErr)
			terminal := true
			if retryable && task.Attempts < maxAttempts {
				terminal = false
				next := now.Add(retryDelay(task.Attempts))
				if err := w.Database.RetryTask(ctx, task.ID, code, "", next); err != nil {
					return err
				}
				result.Retried++
				result.Failures = append(result.Failures, FailureResult{BindingID: binding.ID, TaskID: task.ID, Code: code, Retrying: true})
			} else {
				if err := w.Database.FailTask(ctx, task.ID, code, ""); err != nil {
					return err
				}
				result.Failed++
				result.Failures = append(result.Failures, FailureResult{BindingID: binding.ID, TaskID: task.ID, Code: code})
			}
			if terminal && (policy.NotifyMode == model.NotifyAll || policy.NotifyMode == model.NotifyFailures) {
				if _, err := w.Database.QueueNotification(ctx, model.Notification{CandidateHash: notificationHash(outcome.CandidateHash, task.ID, code), Kind: "failed:" + code, QueuedAt: now}); err != nil {
					return err
				}
			}
			continue
		}
		outcome = w.normalizeOutcome(ctx, outcome)
		if err := w.Database.CompleteTask(ctx, task.ID); err != nil {
			return err
		}
		result.Succeeded++
		result.Results = append(result.Results, BindingResult{BindingID: binding.ID, TaskID: task.ID, Outcome: outcome})
		if kind := successNotification(policy, outcome); kind != "" {
			if _, err := w.Database.QueueNotification(ctx, model.Notification{CandidateHash: notificationHash(outcome.CandidateHash, task.ID, kind), Kind: kind, QueuedAt: now}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Worker) normalizeOutcome(ctx context.Context, outcome Outcome) Outcome {
	if !outcome.Changed || outcome.CandidateID == "" {
		return outcome
	}
	candidate, err := w.Database.GetCandidate(ctx, outcome.CandidateID)
	if err == nil && candidate.Status == model.CandidateSuperseded {
		outcome.Changed = false
	}
	return outcome
}

func (w *Worker) validate() error {
	if w.Database == nil || w.Database.DB() == nil {
		return errors.New("reconcile: database is required")
	}
	if w.Paths.ActivationLock == "" || w.Paths.StateDir == "" || w.Paths.GenerationsDir == "" || w.Paths.RuntimesDir == "" {
		return errors.New("reconcile: state paths are incomplete")
	}
	if w.Coordinator == nil {
		return errors.New("reconcile: coordinator is required")
	}
	return nil
}

func (w *Worker) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}
	return w.Now().UTC()
}

func (w *Worker) idempotencyKey(binding model.Binding, policy model.Policy, signal int64, now time.Time) string {
	interval := w.Config.Check.Interval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	slot := now.UnixNano() / interval.Nanoseconds()
	material := strings.Join([]string{
		"reconcile-v1", binding.ID, binding.ObservedVersion, binding.ObservedHash,
		string(policy.TrackChannel), policy.Constraint, string(policy.ApplyMode), string(policy.LocalCapMode),
		strconv.FormatInt(policy.UpdatedAt.UnixNano(), 10), strconv.FormatInt(slot, 10), strconv.FormatInt(signal, 10),
	}, "\x00")
	hash := sha256.Sum256([]byte(material))
	return "reconcile:" + hex.EncodeToString(hash[:])
}

func scanAndPersist(ctx context.Context, database *store.Store, options InventoryOptions) (inventory.PersistResult, error) {
	home := options.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return inventory.PersistResult{}, err
		}
	}
	report, err := inventory.Scan(ctx, home, options.CurrentProject, options.Projects)
	if err != nil {
		return inventory.PersistResult{}, err
	}
	return inventory.Persist(ctx, database, report)
}

func effectiveMode(global, local model.ApplyMode) model.ApplyMode {
	rank := func(value model.ApplyMode) int {
		switch value {
		case model.ApplyIgnore:
			return 2
		case model.ApplyManual:
			return 1
		default:
			return 0
		}
	}
	if rank(local) > rank(global) {
		return local
	}
	return global
}

func normalizeReason(value string) string {
	switch value {
	case ReasonScheduled, ReasonKick, ReasonCommand:
		return value
	default:
		return ReasonCommand
	}
}

func classifyError(err error) (string, bool) {
	var coded CodedError
	if errors.As(err, &coded) && reasonCodePattern.MatchString(coded.ReasonCode()) {
		return coded.ReasonCode(), coded.Retryable()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "operation_canceled", true
	}
	message := strings.ToLower(err.Error())
	for _, transient := range []string{
		"version lookup failed", "tag lookup failed", "source check failed", "fetch update", "fetch adoption baseline",
		"rebuild exact runtime", "staging failed", "isolated install failed", "registry unavailable",
		"connection reset", "connection refused", "temporarily unavailable", "timed out", "timeout",
		"http 429", "http 500", "http 502", "http 503", "http 504",
	} {
		if strings.Contains(message, transient) {
			return "upstream_unavailable", true
		}
	}
	return "reconcile_failed", false
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(1<<(attempt-1)) * time.Minute
}

func successNotification(policy model.Policy, outcome Outcome) string {
	if policy.NotifyMode == model.NotifyNone {
		return ""
	}
	if outcome.NeedsReview {
		return "needs_review"
	}
	if policy.NotifyMode != model.NotifyAll || !outcome.Changed {
		return ""
	}
	if outcome.Activated {
		return "updated"
	}
	return "update_available"
}

func notificationHash(candidateHash, taskID, kind string) string {
	if len(candidateHash) == sha256.Size*2 {
		if _, err := hex.DecodeString(candidateHash); err == nil {
			return strings.ToLower(candidateHash)
		}
	}
	hash := sha256.Sum256([]byte(taskID + "\x00" + kind))
	return hex.EncodeToString(hash[:])
}

func stableTaskID(key string) string {
	hash := sha256.Sum256([]byte(key))
	return "task_" + hex.EncodeToString(hash[:13])
}

func hookSignal(ctx context.Context, database *store.Store) (int64, error) {
	var value sql.NullInt64
	// The highest durable event ID is an epoch. A new Hook event invalidates the
	// current interval's task key; acknowledging the event must not move the
	// epoch backwards and accidentally enqueue the same work a second time.
	err := database.DB().QueryRowContext(ctx, `SELECT MAX(id) FROM hook_events`).Scan(&value)
	return value.Int64, err
}

func markHookSignalsProcessed(ctx context.Context, database *store.Store, throughID int64, when time.Time) error {
	if throughID <= 0 {
		return nil
	}
	// Do not acknowledge an event that raced in after hookSignal. Its larger ID
	// must remain pending so the next worker gets a new idempotency epoch.
	_, err := database.DB().ExecContext(ctx, `UPDATE hook_events SET processed_at=? WHERE processed_at IS NULL AND id<=?`, when.UTC().Format(time.RFC3339Nano), throughID)
	return err
}
