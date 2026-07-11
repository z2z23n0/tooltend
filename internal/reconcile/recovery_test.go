package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/activation"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestRecoverActivationsCompletesPointerSwitch(t *testing.T) {
	ctx := context.Background()
	database, paths := openWorkerStore(t)
	now := time.Now().UTC()
	bindingID := "recover-binding"
	sourceHash := strings.Repeat("1", 64)
	oldTree := strings.Repeat("2", 64)
	newTree := strings.Repeat("3", 64)
	candidateHash := strings.Repeat("4", 64)
	if err := database.UpsertSource(ctx, model.Source{ID: "recover-source", Kind: model.SourceGit, Locator: "https://example.test/recover", IdentityHash: sourceHash, MetadataJSON: `{}`, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertComponent(ctx, model.LogicalComponent{ID: "recover-component", Kind: model.ComponentSkill, Name: "recover", SourceID: "recover-source", LogicalKey: "recover-key", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertBinding(ctx, model.Binding{ID: bindingID, ComponentID: "recover-component", Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/tmp/recover", Managed: true, Classification: model.ClassificationClean, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{oldTree, newTree} {
		if err := database.PutObjectRecord(ctx, model.ObjectRecord{Hash: hash, Kind: model.ObjectTree, RelativePath: hash, VerifiedAt: now, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.PutGeneration(ctx, model.Generation{ID: "old", BindingID: bindingID, ResolvedRef: "v1", TreeHash: oldTree, State: model.GenerationActive, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.PutGeneration(ctx, model.Generation{ID: "new", BindingID: bindingID, ResolvedRef: "v2", TreeHash: newTree, State: model.GenerationPrepared, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetActiveGeneration(ctx, bindingID, "old"); err != nil {
		t.Fatal(err)
	}
	if err := database.PutCandidate(ctx, model.UpdateCandidate{ID: "recover-candidate", BindingID: bindingID, SourceID: "recover-source", ResolvedRef: "v2", UpstreamTreeHash: newTree, CandidateHash: candidateHash, Status: model.CandidateReady, ReviewClass: model.ReviewNone, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(paths.GenerationsDir, bindingID)
	for generation, body := range map[string]string{"old": "old", "new": "new"} {
		path := filepath.Join(root, "generations", generation)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := activation.SwitchCurrent(root, "old"); err != nil {
		t.Fatal(err)
	}
	newPath, err := activation.GenerationPath(root, "new")
	if err != nil {
		t.Fatal(err)
	}
	expectedHash, err := activation.HashGeneration(newPath)
	if err != nil {
		t.Fatal(err)
	}
	sqlStore, err := activation.NewSQLStore(database)
	if err != nil {
		t.Fatal(err)
	}
	oldPath, err := activation.GenerationPath(root, "old")
	if err != nil {
		t.Fatal(err)
	}
	expectedOldHash, err := activation.HashGeneration(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	intent := activation.Intent{ID: "recover-intent", BindingID: bindingID, CandidateID: "recover-candidate", CandidateHash: candidateHash, OldGeneration: "old", NewGeneration: "new", ExpectedGenerationHash: expectedHash, ExpectedOldGenerationHash: expectedOldHash}
	if err := sqlStore.SaveIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	// Model a crash after the atomic pointer rename but before the phase write.
	if err := activation.SwitchCurrent(root, "new"); err != nil {
		t.Fatal(err)
	}

	count, err := RecoverActivations(ctx, database, paths)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recovered %d intents", count)
	}
	var phase, status, active string
	if err := database.DB().QueryRow(`SELECT phase FROM activation_intents WHERE id='recover-intent'`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if err := database.DB().QueryRow(`SELECT status FROM candidates WHERE id='recover-candidate'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := database.DB().QueryRow(`SELECT active_generation_id FROM bindings WHERE id=?`, bindingID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if phase != "committed" || status != "active" || active != "new" {
		t.Fatalf("phase=%q status=%q active=%q", phase, status, active)
	}
}
