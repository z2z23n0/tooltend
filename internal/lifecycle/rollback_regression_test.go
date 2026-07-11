package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestRollbackIgnoresNeverActivatedPreparedGeneration(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil || !updated.Activated {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	current, err := fixture.database.GetGeneration(context.Background(), binding.ID, binding.ActiveGenerationID)
	if err != nil {
		t.Fatal(err)
	}
	orphanID := "gen_prepared_orphan"
	root := fixture.service.activationRoot(binding.ID, false)
	orphanPath, _ := activation.GenerationPath(root, orphanID)
	if err := fixture.service.Objects.MaterializeTree(context.Background(), current.TreeHash, orphanPath); err != nil {
		t.Fatal(err)
	}
	orphanHash, err := activation.HashGeneration(orphanPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.PutGeneration(context.Background(), model.Generation{
		ID: orphanID, BindingID: binding.ID, ResolvedRef: "future-ref", TreeHash: current.TreeHash,
		IntegrityHash: orphanHash, State: model.GenerationPrepared, CreatedAt: fixture.service.now().AddDate(1, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}

	rolledBack, err := fixture.service.Rollback(context.Background(), "component", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.To != adopted.Generation {
		t.Fatalf("rollback selected %q instead of inactive %q", rolledBack.To, adopted.Generation)
	}
}

func TestRollbackRejectsPreparedOnlyHistory(t *testing.T) {
	service := &Service{}
	_, err := service.selectRollbackGeneration(context.Background(), model.Binding{ActiveGenerationID: "active"}, []model.Generation{
		{ID: "active", State: model.GenerationActive},
		{ID: "prepared", State: model.GenerationPrepared, ResolvedRef: "v0"},
	}, "")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rollback target error=%v", err)
	}
}

func TestRollbackCanTraverseSamePairRepeatedly(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	updated, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{})
	if err != nil || !updated.Activated {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil {
		t.Fatal(err)
	}
	secondGeneration := binding.ActiveGenerationID
	for index, target := range []string{adopted.Generation, secondGeneration, adopted.Generation, secondGeneration} {
		result, err := fixture.service.Rollback(context.Background(), "component", "", target)
		if err != nil {
			t.Fatalf("rollback %d to %s: %v", index+1, target, err)
		}
		if result.To != target {
			t.Fatalf("rollback %d=%+v", index+1, result)
		}
	}
	var activeCandidates int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM candidates WHERE binding_id=? AND status='active'`, binding.ID).Scan(&activeCandidates); err != nil {
		t.Fatal(err)
	}
	if activeCandidates != 1 {
		t.Fatalf("active candidates=%d", activeCandidates)
	}
	active, err := fixture.database.GetGeneration(context.Background(), binding.ID, secondGeneration)
	if err != nil || active.CandidateID == "" {
		t.Fatalf("active generation=%+v err=%v", active, err)
	}
	var status string
	if err := fixture.database.DB().QueryRow(`SELECT status FROM candidates WHERE id=?`, active.CandidateID).Scan(&status); err != nil || status != string(model.CandidateActive) {
		t.Fatalf("candidate status=%q err=%v", status, err)
	}
}

func TestPreJournalActivationFailureRetiresPreparedGeneration(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(fixture.installPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.seedBindingOnly(t, false, model.ApplyAuto)
	if _, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/tooltend-plugin", Version: "v1",
	}); err != nil {
		t.Fatal(err)
	}
	fixture.provider.latest = "v2"
	fixture.service.SnapshotFailpoint = func(point SnapshotFailpoint) error {
		if point == SnapshotAfterCandidateCapture {
			return os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("edit after capture\n"), 0o644)
		}
		return nil
	}
	if _, err := fixture.service.Update(context.Background(), "component", "", UpdateOptions{}); err == nil {
		t.Fatal("expected activation failure")
	}
	var prepared int
	if err := fixture.database.DB().QueryRow(`SELECT count(*) FROM generations WHERE binding_id='binding' AND state='prepared'`).Scan(&prepared); err != nil {
		t.Fatal(err)
	}
	if prepared != 0 {
		t.Fatalf("prepared generations=%d", prepared)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.service.activationRoot("binding", false), "generations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			paths = append(paths, filepath.Join("generations", entry.Name()))
		}
		t.Fatalf("generation directories=%v", paths)
	}
}
