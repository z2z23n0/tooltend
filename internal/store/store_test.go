package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

func TestOpenRWMigratesAndHookDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if _, err := OpenHook(missing); err == nil {
		t.Fatal("OpenHook created missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: %v", err)
	}

	path := filepath.Join(dir, "state", "state.db")
	store, err := OpenRW(path)
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.UserVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("version = %d", version)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	hook, err := OpenHook(path)
	if err != nil {
		t.Fatal(err)
	}
	defer hook.Close()
	var timeout int
	if err := hook.DB().QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 0 {
		t.Fatalf("hook busy_timeout = %d", timeout)
	}
}

func TestInventoryCandidateAndReview(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	hashC := strings.Repeat("c", 64)
	if err := s.UpsertSource(ctx, model.Source{ID: "src", Kind: model.SourceGit, Locator: "https://example.test/repo", IdentityHash: hashA, MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertComponent(ctx, model.LogicalComponent{ID: "cmp", Kind: model.ComponentSkill, Name: "skill", SourceID: "src", LogicalKey: "skill:git:repo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBinding(ctx, model.Binding{ID: "bnd", ComponentID: "cmp", Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/tmp/skill", Classification: model.ClassificationUnknown, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	policy := model.DefaultPolicy()
	policy.BindingID = "bnd"
	policy.UpdatedAt = now
	if err := s.SetPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	nextPolicy := policy
	nextPolicy.ApplyMode = model.ApplyManual
	nextPolicy.UpdatedAt = now.Add(time.Second)
	if err := s.CompareAndSetPolicy(ctx, policy, nextPolicy); err != nil {
		t.Fatal(err)
	}
	stalePolicy := nextPolicy
	stalePolicy.NotifyMode = model.NotifyAll
	stalePolicy.UpdatedAt = now.Add(2 * time.Second)
	if err := s.CompareAndSetPolicy(ctx, policy, stalePolicy); err == nil {
		t.Fatal("stale policy compare-and-set overwrote newer policy")
	}
	if err := s.PutObjectRecord(ctx, model.ObjectRecord{Hash: hashB, Kind: model.ObjectTree, Size: 1, RelativePath: hashB, VerifiedAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObjectRecord(ctx, model.ObjectRecord{Hash: hashC, Kind: model.ObjectBundle, Size: 1, RelativePath: hashC, VerifiedAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	candidate := model.UpdateCandidate{ID: "cand", BindingID: "bnd", SourceID: "src", ResolvedRef: "v1", UpstreamTreeHash: hashB, CandidateHash: hashA, Status: model.CandidateAvailable, ReviewClass: model.ReviewSemanticSkillOnly, CreatedAt: now, UpdatedAt: now}
	if err := s.PutCandidate(ctx, candidate); err != nil {
		t.Fatal(err)
	}
	for _, status := range []model.CandidateStatus{model.CandidateStaging, model.CandidateVerified, model.CandidateMerging, model.CandidateValidating} {
		if err := s.TransitionCandidate(ctx, "cand", status, "", ""); err != nil {
			t.Fatalf("transition to %s: %v", status, err)
		}
	}
	bundle := model.ReviewBundle{ID: "bundle", CandidateID: "cand", CandidateHash: hashA, ObjectHash: hashC, RiskTypesJSON: `["semantic"]`, CreatedAt: now}
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_review_bundle BEFORE INSERT ON review_bundles BEGIN SELECT RAISE(ABORT, 'bundle failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidateWithReviewBundle(ctx, "cand", "review_required", "", bundle); err == nil {
		t.Fatal("expected atomic review transition failure")
	}
	if candidateAfterFailure, err := s.GetCandidate(ctx, "cand"); err != nil || candidateAfterFailure.Status != model.CandidateValidating {
		t.Fatalf("candidate after failure=%+v err=%v", candidateAfterFailure, err)
	}
	if _, err := s.GetReviewBundle(ctx, "cand"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("review bundle after failure err=%v", err)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER fail_review_bundle`); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionCandidateWithReviewBundle(ctx, "cand", "review_required", "", bundle); err != nil {
		t.Fatal(err)
	}
	review := model.Review{ID: "rev", CandidateID: "cand", CandidateHash: hashA, ActorType: model.ActorAgent, Verdict: model.VerdictSafe, RiskType: "semantic", Summary: "safe", CreatedAt: now}
	if err := s.SubmitReview(ctx, review); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCandidate(ctx, "cand")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.CandidateReady {
		t.Fatalf("status = %s", got.Status)
	}
	review.ID = "bad"
	review.CandidateHash = hashB
	if err := s.SubmitReview(ctx, review); err == nil {
		t.Fatal("expected hash mismatch")
	}
}

func TestTaskAndNotificationDeduplication(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	task := model.Task{ID: "task", Kind: "scan", IdempotencyKey: "scan:global:x", Status: model.TaskPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	inserted, err := s.EnqueueTask(ctx, task)
	if err != nil || !inserted {
		t.Fatalf("enqueue: %v %v", inserted, err)
	}
	task.ID = "other"
	inserted, err = s.EnqueueTask(ctx, task)
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue: %v %v", inserted, err)
	}
	claimed, err := s.ClaimTask(ctx, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != model.TaskRunning || claimed.Attempts != 1 {
		t.Fatalf("claimed = %#v", claimed)
	}
	if err := s.CompleteTask(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("c", 64)
	queued, err := s.QueueNotification(ctx, model.Notification{CandidateHash: hash, Kind: "failure", QueuedAt: now})
	if err != nil || !queued {
		t.Fatalf("queue: %v %v", queued, err)
	}
	queued, err = s.QueueNotification(ctx, model.Notification{CandidateHash: hash, Kind: "failure", QueuedAt: now})
	if err != nil || queued {
		t.Fatalf("dedup: %v %v", queued, err)
	}
	notifications, err := s.TakeNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 {
		t.Fatalf("notifications = %d", len(notifications))
	}
	notifications, err = s.TakeNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 0 {
		t.Fatalf("notification shown twice")
	}
}

func TestHookEventDoesNotHaveRawSensitiveColumns(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	columns, err := s.DB().QueryContext(ctx, `PRAGMA table_info(hook_events)`)
	if err != nil {
		t.Fatal(err)
	}
	defer columns.Close()
	for columns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "command", "argv", "environment", "prompt", "transcript", "secret", "token":
			t.Fatalf("sensitive raw column %q exists", name)
		}
	}
}

func TestCommitAdoptionRollsBackEveryMutableRowOnLateFailure(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	oldIdentity := strings.Repeat("a", 64)
	newIdentity := strings.Repeat("b", 64)
	treeHash := strings.Repeat("c", 64)
	oldSource := model.Source{
		ID: "old-source", Kind: model.SourceGit, Locator: "https://example.test/old", IdentityHash: oldIdentity,
		MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.UpsertSource(ctx, oldSource); err != nil {
		t.Fatal(err)
	}
	expectedComponent := model.LogicalComponent{
		ID: "component", Kind: model.ComponentPlugin, Name: "component", SourceID: oldSource.ID,
		LogicalKey: "plugin:component", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.UpsertComponent(ctx, expectedComponent); err != nil {
		t.Fatal(err)
	}
	expectedBinding := model.Binding{
		ID: "binding", ComponentID: expectedComponent.ID, Host: model.HostCodex, Scope: model.ScopeGlobal,
		InstallPath: "/tmp/tooltend-adoption", InstallMethod: "native", Classification: model.ClassificationUnknown,
		LastSeenAt: now,
	}
	if err := s.UpsertBinding(ctx, expectedBinding); err != nil {
		t.Fatal(err)
	}
	if err := s.PutObjectRecord(ctx, model.ObjectRecord{
		Hash: treeHash, Kind: model.ObjectTree, RelativePath: treeHash, VerifiedAt: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `CREATE TRIGGER fail_adoption_receipt BEFORE INSERT ON receipts BEGIN SELECT RAISE(ABORT, 'late receipt failure'); END`); err != nil {
		t.Fatal(err)
	}

	source := model.Source{
		ID: "new-source", Kind: model.SourceGit, Locator: "https://example.test/new", IdentityHash: newIdentity,
		MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	component := expectedComponent
	component.SourceID = source.ID
	component.UpdatedAt = now.Add(time.Second)
	binding := expectedBinding
	binding.Managed = true
	binding.InstallMethod = "tooltend-generation"
	binding.Classification = model.ClassificationClean
	binding.ObservedHash = treeHash
	binding.ActiveGenerationID = "generation"
	binding.LastSeenAt = now.Add(time.Second)
	err := s.CommitAdoption(ctx, AdoptionCommit{
		Source:            source,
		Trust:             model.SourceTrust{SourceID: source.ID, Level: model.TrustVerified, ApprovedBy: "explicit_adopt", ApprovedAt: now},
		ExpectedComponent: expectedComponent,
		Component:         component,
		ExpectedBinding:   expectedBinding,
		Binding:           binding,
		Generation: model.Generation{
			ID: "generation", BindingID: binding.ID, ResolvedRef: "v1", TreeHash: treeHash,
			IntegrityHash: treeHash, State: model.GenerationOriginal, CreatedAt: now,
		},
		Baseline: &model.Baseline{
			ID: "baseline", BindingID: binding.ID, SourceID: source.ID, ResolvedRef: "v1", TreeHash: treeHash, CreatedAt: now,
		},
		Receipt: model.Receipt{
			ID: "receipt", BindingID: binding.ID, Action: model.ReceiptAdopt, NewGenerationID: "generation",
			ToRef: "v1", Status: model.ReceiptSucceeded, SummaryJSON: "{}", CreatedAt: now,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "late receipt failure") {
		t.Fatalf("late adoption failure = %v", err)
	}

	assertCount := func(query string, expected int) {
		t.Helper()
		var count int
		if err := s.DB().QueryRowContext(ctx, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != expected {
			t.Fatalf("%s: count=%d want=%d", query, count, expected)
		}
	}
	assertCount(`SELECT count(*) FROM sources WHERE id='new-source'`, 0)
	assertCount(`SELECT count(*) FROM source_trust WHERE source_id='new-source'`, 0)
	assertCount(`SELECT count(*) FROM generations WHERE id='generation'`, 0)
	assertCount(`SELECT count(*) FROM baselines WHERE id='baseline'`, 0)
	assertCount(`SELECT count(*) FROM receipts WHERE id='receipt'`, 0)
	storedComponent, err := s.ListComponents(ctx)
	if err != nil || len(storedComponent) != 1 || storedComponent[0].SourceID != oldSource.ID {
		t.Fatalf("component changed after rollback: %+v err=%v", storedComponent, err)
	}
	storedBinding, err := s.GetBinding(ctx, expectedBinding.ID)
	if err != nil || storedBinding.Managed || storedBinding.ActiveGenerationID != "" || storedBinding.InstallMethod != expectedBinding.InstallMethod {
		t.Fatalf("binding changed after rollback: %+v err=%v", storedBinding, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
