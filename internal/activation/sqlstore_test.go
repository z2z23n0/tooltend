package activation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	storepkg "github.com/z2z23n0/tooltend/internal/store"
)

func TestSQLStoreActivationIsAtomicAndIdempotent(t *testing.T) {
	root, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)
	sqlStore.now = func() time.Time { return now }
	manager := Manager{Root: root, Store: sqlStore, Now: func() time.Time { return now }}

	first, err := manager.Activate(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Activate(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || !first.ActivatedAt.Equal(second.ActivatedAt) {
		t.Fatalf("first=%+v second=%+v", first, second)
	}

	assertDatabaseValue(t, database, `SELECT phase FROM activation_intents WHERE id=?`, intent.ID, "committed")
	assertDatabaseValue(t, database, `SELECT status FROM candidates WHERE id=?`, intent.CandidateID, "active")
	assertDatabaseValue(t, database, `SELECT active_generation_id FROM bindings WHERE id=?`, intent.BindingID, intent.NewGeneration)
	assertDatabaseValue(t, database, `SELECT state FROM generations WHERE id=?`, intent.OldGeneration, "inactive")
	assertDatabaseValue(t, database, `SELECT state FROM generations WHERE id=?`, intent.NewGeneration, "active")
	assertDatabaseValue(t, database, `SELECT CAST(count(*) AS TEXT) FROM receipts WHERE id=?`, intent.ID, "1")
}

func TestSQLStorePersistsPointerMarkerAndRecovers(t *testing.T) {
	root, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("crash after pointer journal")
	manager := Manager{
		Root:  root,
		Store: sqlStore,
		Failpoint: func(point Failpoint) error {
			if point == FailAfterPointerPersist {
				return crash
			}
			return nil
		},
	}
	if _, err := manager.Activate(context.Background(), intent); !errors.Is(err, crash) {
		t.Fatalf("got %v", err)
	}
	assertDatabaseValue(t, database, `SELECT phase FROM activation_intents WHERE id=?`, intent.ID, string(PhasePointerSwitched))

	results, err := (&Manager{Root: root, Store: sqlStore}).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != RecoveryCommitted {
		t.Fatalf("results=%+v", results)
	}
	assertDatabaseValue(t, database, `SELECT phase FROM activation_intents WHERE id=?`, intent.ID, "committed")
}

func TestSQLStoreRollbackIsTerminalAndReceipted(t *testing.T) {
	root, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	healthCalls := 0
	manager := Manager{
		Root:  root,
		Store: sqlStore,
		Health: func(context.Context, string) error {
			healthCalls++
			if healthCalls == 2 {
				return errors.New("bad generation")
			}
			return nil
		},
	}
	if _, err := manager.Activate(context.Background(), intent); err == nil {
		t.Fatal("expected health failure")
	}
	assertDatabaseValue(t, database, `SELECT phase FROM activation_intents WHERE id=?`, intent.ID, "rolled_back")
	assertDatabaseValue(t, database, `SELECT status FROM candidates WHERE id=?`, intent.CandidateID, "rolled_back")
	assertDatabaseValue(t, database, `SELECT state FROM generations WHERE id=?`, intent.NewGeneration, "failed")
	assertDatabaseValue(t, database, `SELECT CAST(count(*) AS TEXT) FROM receipts WHERE id=?`, intent.ID+"-rollback", "1")
	if current, err := Current(root); err != nil || current != intent.OldGeneration {
		t.Fatalf("current=%q err=%v", current, err)
	}
}

func TestSQLStoreRestoreFailureDoesNotCommitRollbackState(t *testing.T) {
	root, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	healthCalls := 0
	manager := Manager{
		Root:  root,
		Store: sqlStore,
		Health: func(context.Context, string) error {
			healthCalls++
			if healthCalls == 2 {
				oldPath, _ := GenerationPath(root, intent.OldGeneration)
				if err := os.RemoveAll(oldPath); err != nil {
					return err
				}
				return errors.New("bad generation")
			}
			return nil
		},
	}
	if _, err := manager.Activate(context.Background(), intent); err == nil || !strings.Contains(err.Error(), "restore old generation") {
		t.Fatalf("activation error=%v", err)
	}
	assertDatabaseValue(t, database, `SELECT phase FROM activation_intents WHERE id=?`, intent.ID, "pointer_switched")
	assertDatabaseValue(t, database, `SELECT status FROM candidates WHERE id=?`, intent.CandidateID, "activating")
	assertDatabaseValue(t, database, `SELECT state FROM generations WHERE id=?`, intent.NewGeneration, "prepared")
	assertDatabaseValue(t, database, `SELECT CAST(count(*) AS TEXT) FROM receipts WHERE id=?`, intent.ID+"-rollback", "0")
	if current, err := Current(root); err != nil || current != intent.NewGeneration {
		t.Fatalf("current=%q err=%v", current, err)
	}
}

func TestSQLStoreCompletesRollbackMetadataInActivationTransaction(t *testing.T) {
	root, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	now := time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)
	oldCandidateHash := strings.Repeat("d", 64)
	newTree := strings.Repeat("b", 64)
	if _, err := database.DB().Exec(`INSERT INTO candidates(id,binding_id,source_id,resolved_ref,upstream_tree_hash,candidate_hash,status,review_class,created_at,updated_at)
		VALUES('old-candidate',?,'source-1','old-ref',?,?,'active','none',?,?)`, intent.BindingID, strings.Repeat("a", 64), oldCandidateHash, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().Exec(`UPDATE generations SET candidate_id='old-candidate' WHERE id=?`, intent.OldGeneration); err != nil {
		t.Fatal(err)
	}
	intent.Completion = Completion{
		Action: CompletionRollback, FromRef: "old-ref", ToRef: "new-ref",
		Baseline:              &BaselineCompletion{ID: "rollback-baseline", SourceID: "source-1", ResolvedRef: "new-ref", TreeHash: newTree},
		RolledBackCandidateID: "old-candidate",
	}
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	sqlStore.now = func() time.Time { return now }
	receipt, err := (&Manager{Root: root, Store: sqlStore, Now: func() time.Time { return now }}).Activate(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != CompletionRollback || receipt.FromRef != "old-ref" || receipt.ToRef != "new-ref" {
		t.Fatalf("receipt=%+v", receipt)
	}
	assertDatabaseValue(t, database, `SELECT action FROM receipts WHERE id=?`, intent.ID, "rollback")
	assertDatabaseValue(t, database, `SELECT status FROM candidates WHERE id=?`, "old-candidate", "rolled_back")
	assertDatabaseValue(t, database, `SELECT tree_hash FROM baselines WHERE id=?`, "rollback-baseline", newTree)
	assertDatabaseValue(t, database, `SELECT candidate_id FROM generations WHERE id=?`, intent.NewGeneration, intent.CandidateID)
}

func TestSQLStoreRollbackCompletionFailureRemainsRecoverable(t *testing.T) {
	root, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	newTree := strings.Repeat("b", 64)
	intent.Completion = Completion{
		Action: CompletionRollback, FromRef: "old-ref", ToRef: "new-ref",
		Baseline: &BaselineCompletion{ID: "rollback-baseline", SourceID: "source-1", ResolvedRef: "new-ref", TreeHash: newTree},
	}
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().Exec(`CREATE TRIGGER fail_rollback_baseline BEFORE INSERT ON baselines BEGIN SELECT RAISE(ABORT, 'baseline failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = (&Manager{Root: root, Store: sqlStore}).Activate(context.Background(), intent)
	if err == nil || !strings.Contains(err.Error(), "baseline failure") {
		t.Fatalf("activation error=%v", err)
	}
	assertDatabaseValue(t, database, `SELECT phase FROM activation_intents WHERE id=?`, intent.ID, "pointer_switched")
	assertDatabaseValue(t, database, `SELECT active_generation_id FROM bindings WHERE id=?`, intent.BindingID, intent.OldGeneration)
	assertDatabaseValue(t, database, `SELECT status FROM candidates WHERE id=?`, intent.CandidateID, "activating")
	assertDatabaseValue(t, database, `SELECT CAST(count(*) AS TEXT) FROM receipts WHERE id=?`, intent.ID, "0")
	if _, err := database.DB().Exec(`DROP TRIGGER fail_rollback_baseline`); err != nil {
		t.Fatal(err)
	}
	results, err := (&Manager{Root: root, Store: sqlStore}).Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Action != RecoveryCommitted {
		t.Fatalf("results=%+v", results)
	}
	assertDatabaseValue(t, database, `SELECT action FROM receipts WHERE id=?`, intent.ID, "rollback")
	assertDatabaseValue(t, database, `SELECT tree_hash FROM baselines WHERE id=?`, "rollback-baseline", newTree)
}

func TestSQLStoreRejectsStaleOrConcurrentBindingIntent(t *testing.T) {
	_, intent := activationFixture(t)
	database := seededActivationStore(t, intent)
	sqlStore, err := NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	stale := intent
	stale.ID = "stale-intent"
	stale.OldGeneration = "missing-old"
	if err := sqlStore.SaveIntent(context.Background(), stale); err == nil || !strings.Contains(err.Error(), "active generation changed") {
		t.Fatalf("stale intent error=%v", err)
	}
	if err := sqlStore.SaveIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	second := intent
	second.ID = "second-intent"
	if err := sqlStore.SaveIntent(context.Background(), second); err == nil {
		t.Fatal("second unfinished intent for one binding was accepted")
	}
}

func seededActivationStore(t *testing.T, intent Intent) *storepkg.Store {
	t.Helper()
	database, err := storepkg.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	oldTree := strings.Repeat("a", 64)
	newTree := strings.Repeat("b", 64)
	candidateHash := intent.CandidateHash

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO sources(id,kind,locator,identity_hash,metadata_json,created_at,updated_at) VALUES('source-1','git','https://example.invalid/repo','source-hash','{}',?,?)`, []any{now, now}},
		{`INSERT INTO components(id,kind,name,source_id,logical_key,created_at,updated_at) VALUES('component-1','skill','test','source-1','git:test',?,?)`, []any{now, now}},
		{`INSERT INTO bindings(id,component_id,host,scope,install_path,managed,classification,active_generation_id,last_seen_at) VALUES(?,'component-1','codex','global','/tmp/tooltend-test',1,'clean',?,?)`, []any{intent.BindingID, intent.OldGeneration, now}},
		{`INSERT INTO objects(hash,kind,size,relative_path,verified_at,created_at) VALUES(?,'tree',0,'old',?,?),(?,'tree',0,'new',?,?)`, []any{oldTree, now, now, newTree, now, now}},
		{`INSERT INTO generations(id,binding_id,resolved_ref,tree_hash,state,created_at,activated_at) VALUES(?,?, 'old-ref',?,'active',?,?),(?,?,'new-ref',?,'prepared',?,NULL)`, []any{intent.OldGeneration, intent.BindingID, oldTree, now, now, intent.NewGeneration, intent.BindingID, newTree, now}},
		{`INSERT INTO candidates(id,binding_id,source_id,resolved_ref,upstream_tree_hash,candidate_hash,status,review_class,created_at,updated_at) VALUES(?,?,'source-1','new-ref',?,?,'ready','none',?,?)`, []any{intent.CandidateID, intent.BindingID, newTree, candidateHash, now, now}},
	}
	for _, statement := range statements {
		if _, err := database.DB().Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed database: %v", err)
		}
	}
	return database
}

func assertDatabaseValue(t *testing.T, database *storepkg.Store, query string, argument any, expected string) {
	t.Helper()
	var actual string
	if err := database.DB().QueryRow(query, argument).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("query %q: got %q, want %q", query, actual, expected)
	}
}
