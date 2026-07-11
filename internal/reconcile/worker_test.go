package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestRunOnceAppliesPolicyWithoutElevatingUnmanagedBindings(t *testing.T) {
	ctx := context.Background()
	database, paths := openWorkerStore(t)
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	seedBinding(t, database, "auto-managed", true, model.ApplyAuto, model.ApplyAuto, now)
	seedBinding(t, database, "auto-native", false, model.ApplyAuto, model.ApplyAuto, now)
	seedBinding(t, database, "manual", true, model.ApplyManual, model.ApplyAuto, now)
	seedBinding(t, database, "local-cap", true, model.ApplyAuto, model.ApplyManual, now)
	seedBinding(t, database, "ignored", true, model.ApplyIgnore, model.ApplyAuto, now)
	if _, err := database.RecordHookEvent(ctx, model.HookEvent{OccurredAt: now, Host: model.HostCodex, EventType: "PostToolUse", CorrelationHash: digestForTest("event")}); err != nil {
		t.Fatal(err)
	}

	var requests []Request
	recovered := false
	cfg := config.Default()
	cfg.Projects = []string{"/selected/project"}
	worker := Worker{
		Database: database, Paths: paths, Config: cfg, CurrentProject: "/scheduler/cwd", Now: func() time.Time { return now },
		Recover: func(context.Context, *store.Store, config.Paths) (int, error) {
			recovered = true
			return 2, nil
		},
		Inventory: func(_ context.Context, _ *store.Store, options InventoryOptions) (inventory.PersistResult, error) {
			if !recovered {
				t.Fatal("inventory ran before activation recovery")
			}
			if options.CurrentProject != "" || len(options.Projects) != 1 || options.Projects[0] != "/selected/project" {
				t.Fatalf("worker scanned outside selected projects: %#v", options)
			}
			return inventory.PersistResult{Bindings: 5}, nil
		},
		Coordinator: CoordinatorFunc(func(_ context.Context, request Request) (Outcome, error) {
			requests = append(requests, request)
			return Outcome{CandidateHash: digestForTest(request.Binding.ID), Checked: true, Changed: true, Activated: request.Activate}, nil
		}),
	}
	result, err := worker.RunOnce(ctx, ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recovered != 2 || result.Scheduled != 4 || result.Succeeded != 4 || result.Skipped != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].Binding.ID < requests[j].Binding.ID })
	if len(requests) != 4 {
		t.Fatalf("got %d requests: %#v", len(requests), requests)
	}
	for _, request := range requests {
		wantAuto := request.Binding.ID == "auto-managed"
		if request.Stage != wantAuto || request.Activate != wantAuto {
			t.Errorf("binding %s stage/activate = %v/%v, want %v", request.Binding.ID, request.Stage, request.Activate, wantAuto)
		}
	}

	// The same interval and hook-signal epoch is idempotent even when another
	// one-shot invocation is requested.
	second, err := worker.RunOnce(ctx, ReasonKick)
	if err != nil {
		t.Fatal(err)
	}
	if second.Scheduled != 0 || second.Succeeded != 0 || len(requests) != 4 {
		t.Fatalf("duplicate work was run: result=%#v requests=%d", second, len(requests))
	}
	var pending int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM hook_events WHERE processed_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("hook signal was not acknowledged: pending=%d err=%v", pending, err)
	}
}

func TestRunOnceDoesNotReportSupersededCandidateAsChanged(t *testing.T) {
	ctx := context.Background()
	database, paths := openWorkerStore(t)
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	seedBinding(t, database, "superseded", true, model.ApplyAuto, model.ApplyAuto, now)
	candidateHash := digestForTest("superseded-candidate")
	if err := database.PutCandidate(ctx, model.UpdateCandidate{
		ID: "candidate-superseded", BindingID: "superseded", SourceID: "src_superseded",
		ResolvedRef: "v1", CandidateHash: candidateHash, Status: model.CandidateSuperseded,
		ReviewClass: model.ReviewNone, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	worker := Worker{
		Database: database, Paths: paths, Config: config.Default(), Now: func() time.Time { return now },
		Recover: func(context.Context, *store.Store, config.Paths) (int, error) { return 0, nil },
		Inventory: func(context.Context, *store.Store, InventoryOptions) (inventory.PersistResult, error) {
			return inventory.PersistResult{}, nil
		},
		Coordinator: CoordinatorFunc(func(context.Context, Request) (Outcome, error) {
			return Outcome{CandidateID: "candidate-superseded", CandidateHash: candidateHash, Checked: true, Changed: true}, nil
		}),
	}
	result, err := worker.RunOnce(ctx, ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Outcome.Changed {
		t.Fatalf("superseded candidate reported as changed: %#v", result.Results)
	}
}

func TestRunOnceStoresOnlyCodedFailureAndRetries(t *testing.T) {
	ctx := context.Background()
	database, paths := openWorkerStore(t)
	now := time.Date(2026, 7, 12, 3, 0, 0, 0, time.UTC)
	seedBinding(t, database, "retry", true, model.ApplyAuto, model.ApplyAuto, now)
	attempts := 0
	worker := Worker{
		Database: database, Paths: paths, Config: config.Default(), Now: func() time.Time { return now },
		Recover: func(context.Context, *store.Store, config.Paths) (int, error) { return 0, nil },
		Inventory: func(context.Context, *store.Store, InventoryOptions) (inventory.PersistResult, error) {
			return inventory.PersistResult{}, nil
		},
		Coordinator: CoordinatorFunc(func(context.Context, Request) (Outcome, error) {
			attempts++
			if attempts == 1 {
				return Outcome{}, NewCodedError("registry_unavailable", true)
			}
			return Outcome{Checked: true}, nil
		}),
	}
	first, err := worker.RunOnce(ctx, ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if first.Retried != 1 || first.Failed != 0 {
		t.Fatalf("unexpected retry result: %#v", first)
	}
	var code, summary, status string
	if err := database.DB().QueryRow(`SELECT error_code,error_summary,status FROM tasks WHERE binding_id='retry'`).Scan(&code, &summary, &status); err != nil {
		t.Fatal(err)
	}
	if code != "registry_unavailable" || summary != "" || status != "pending" {
		t.Fatalf("unsafe task failure: code=%q summary=%q status=%q", code, summary, status)
	}
	var notifications int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&notifications); err != nil || notifications != 0 {
		t.Fatalf("transient retry produced a notification: count=%d err=%v", notifications, err)
	}

	now = now.Add(2 * time.Minute)
	second, err := worker.RunOnce(ctx, ReasonScheduled)
	if err != nil {
		t.Fatal(err)
	}
	if second.Succeeded != 1 || attempts != 2 {
		t.Fatalf("retry did not finish: %#v attempts=%d", second, attempts)
	}
}

func TestClassifySanitizedUpstreamFailuresAsRetryable(t *testing.T) {
	for _, err := range []error{
		errors.New("lifecycle: resolve update: npm version lookup failed"),
		errors.New("lifecycle: fetch update: git staging failed"),
		errors.New("lifecycle: resolve update: PyPI version lookup returned HTTP 503"),
	} {
		code, retryable := classifyError(err)
		if code != "upstream_unavailable" || !retryable {
			t.Fatalf("error %q classified as %s retryable=%v", err, code, retryable)
		}
	}
	code, retryable := classifyError(errors.New("lifecycle: candidate hash mismatch"))
	if code != "reconcile_failed" || retryable {
		t.Fatalf("deterministic error classified as %s retryable=%v", code, retryable)
	}
}

func TestRunOnceNonBlockingLockLeavesActiveMarker(t *testing.T) {
	database, paths := openWorkerStore(t)
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(paths.StateDir, "kick.pending")
	if err := os.WriteFile(marker, []byte("queued"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := lockfile.Try(paths.ActivationLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	worker := Worker{Database: database, Paths: paths, Coordinator: CoordinatorFunc(func(context.Context, Request) (Outcome, error) {
		return Outcome{}, errors.New("must not run")
	})}
	result, err := worker.RunOnce(context.Background(), ReasonKick)
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyRunning {
		t.Fatalf("expected non-blocking lock result: %#v", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("active worker marker was cleared: %v", err)
	}
}

func openWorkerStore(t *testing.T) (*store.Store, config.Paths) {
	t.Helper()
	root := t.TempDir()
	paths := config.ResolveWith(root, func(name string) string {
		if name == config.EnvHome {
			return filepath.Join(root, "tooltend")
		}
		return ""
	})
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, paths
}

func seedBinding(t *testing.T, database *store.Store, id string, managed bool, apply, cap model.ApplyMode, now time.Time) {
	t.Helper()
	ctx := context.Background()
	sourceID := "src_" + id
	if err := database.UpsertSource(ctx, model.Source{ID: sourceID, Kind: model.SourceGit, Locator: "https://example.test/" + id, IdentityHash: digestForTest(sourceID), MetadataJSON: `{}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	componentID := "cmp_" + id
	if err := database.UpsertComponent(ctx, model.LogicalComponent{ID: componentID, Kind: model.ComponentSkill, Name: id, SourceID: sourceID, LogicalKey: digestForTest(componentID), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertBinding(ctx, model.Binding{ID: id, ComponentID: componentID, Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/tmp/" + id, Managed: managed, Classification: model.ClassificationClean, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetPolicy(ctx, model.Policy{BindingID: id, TrackChannel: model.TrackStable, ApplyMode: apply, NotifyMode: model.NotifyFailures, LocalCapMode: cap, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

func digestForTest(value string) string {
	return notificationHash("", value, "test")
}
