package project

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestSyncIsProjectScopedAndCannotOverrideLocalExactPin(t *testing.T) {
	ctx := context.Background()
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	root := filepath.Join(t.TempDir(), "current")
	other := filepath.Join(t.TempDir(), "other")
	for _, project := range []model.Project{
		{ID: "p-current", RootPath: root, RootFingerprint: "fp-current", Selected: true, LastSeenAt: now},
		{ID: "p-other", RootPath: other, RootFingerprint: "fp-other", Selected: true, LastSeenAt: now},
	} {
		if err := database.UpsertProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	source := model.Source{ID: "src", Kind: model.SourceNPM, Locator: "https://registry.npmjs.org/example", PackageName: "example", IdentityHash: "source-identity", MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{ID: "cmp", Kind: model.ComponentCLI, Name: "example", SourceID: source.ID, LogicalKey: "npm-example", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	bindings := []model.Binding{
		{ID: "current", ComponentID: component.ID, Host: model.HostCodex, ProjectID: "p-current", Scope: model.ScopeProject, InstallPath: filepath.Join(root, "tool"), Classification: model.ClassificationUnknown, LastSeenAt: now},
		{ID: "other", ComponentID: component.ID, Host: model.HostCodex, ProjectID: "p-other", Scope: model.ScopeProject, InstallPath: filepath.Join(other, "tool"), Classification: model.ClassificationUnknown, LastSeenAt: now},
		{ID: "global", ComponentID: component.ID, Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/tmp/global-tool", Classification: model.ClassificationUnknown, LastSeenAt: now},
	}
	for _, binding := range bindings {
		if err := database.UpsertBinding(ctx, binding); err != nil {
			t.Fatal(err)
		}
		policy := model.DefaultPolicy()
		policy.BindingID, policy.UpdatedAt = binding.ID, now
		if err := database.SetPolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
	manifest := config.ProjectManifest{Version: 1, Components: []config.ProjectComponent{{
		Name: "example", Source: "npm:example", Kind: model.ComponentCLI,
		Agents: []model.HostKind{model.HostCodex}, Track: model.TrackExact, Constraint: "2.0.0",
	}}}
	if err := config.SaveProjectManifestAtomic(filepath.Join(root, ManifestName), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(ctx, database, root, true); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]model.TrackChannel{"current": model.TrackExact, "other": model.TrackStable, "global": model.TrackStable} {
		policy, err := database.GetPolicy(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if policy.TrackChannel != want {
			t.Fatalf("binding %s track=%s want=%s", id, policy.TrackChannel, want)
		}
	}

	local, err := database.GetPolicy(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	local.Constraint = "1.5.0"
	if err := database.SetPolicy(ctx, local); err != nil {
		t.Fatal(err)
	}
	result, err := Sync(ctx, database, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 {
		t.Fatalf("warnings=%#v", result.Warnings)
	}
	local, _ = database.GetPolicy(ctx, "current")
	if local.Constraint != "1.5.0" {
		t.Fatalf("local exact pin was overridden: %#v", local)
	}
	frozen, err := Sync(ctx, database, root, false)
	if err != nil {
		t.Fatal(err)
	}
	local.TrackChannel, local.Constraint, local.ExpectedIntegrity, local.UpdatedAt = model.TrackStable, "", "", time.Now().UTC()
	if err := database.SetPolicy(ctx, local); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplySyncPreview(ctx, database, frozen); err == nil {
		t.Fatal("concurrent loosening created an unpreviewed sync change")
	}
	local, _ = database.GetPolicy(ctx, "current")
	if local.TrackChannel != model.TrackStable || local.Constraint != "" {
		t.Fatalf("rejected frozen preview still changed policy: %#v", local)
	}
}

func TestSyncConsumesLockWithoutElevatingLocalApplyPolicy(t *testing.T) {
	ctx := context.Background()
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	root := t.TempDir()
	if err := database.UpsertProject(ctx, model.Project{ID: "project", RootPath: root, RootFingerprint: "fp", Selected: true, LastSeenAt: now}); err != nil {
		t.Fatal(err)
	}
	source := model.Source{ID: "source", Kind: model.SourceNPM, Locator: "example", PackageName: "example", IdentityHash: strings.Repeat("1", 64), MetadataJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{ID: "component", Kind: model.ComponentCLI, Name: "example", SourceID: source.ID, LogicalKey: "npm-example", CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{ID: "binding", ComponentID: component.ID, Host: model.HostCodex, ProjectID: "project", Scope: model.ScopeProject, InstallPath: filepath.Join(root, "example"), Classification: model.ClassificationClean, LastSeenAt: now}
	if err := database.UpsertBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	policy := model.Policy{BindingID: binding.ID, TrackChannel: model.TrackStable, ApplyMode: model.ApplyManual, NotifyMode: model.NotifyNone, LocalCapMode: model.ApplyIgnore, UpdatedAt: now}
	if err := database.SetPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	manifest := config.ProjectManifest{Version: 1, Components: []config.ProjectComponent{{
		Name: component.Name, Source: "npm:example", Kind: component.Kind,
		Agents: []model.HostKind{model.HostCodex}, Track: model.TrackStable,
	}}}
	if err := config.SaveProjectManifestAtomic(filepath.Join(root, ManifestName), manifest); err != nil {
		t.Fatal(err)
	}
	integrity := strings.Repeat("a", 64)
	lock := config.ProjectLock{Version: 1, Components: []config.ProjectLockComponent{{
		LogicalKey: component.LogicalKey, ResolvedVersion: "2.4.1", Integrity: integrity,
	}}}
	if err := config.SaveProjectLockAtomic(filepath.Join(root, LockName), lock); err != nil {
		t.Fatal(err)
	}
	preview, err := Sync(ctx, database, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 1 || preview.Changes[0].NewConstraint != "2.4.1" || preview.Changes[0].NewIntegrity != integrity {
		t.Fatalf("lock preview=%#v", preview)
	}
	unchanged, err := database.GetPolicy(ctx, binding.ID)
	if err != nil || unchanged.TrackChannel != model.TrackStable {
		t.Fatalf("dry-run changed policy=%#v err=%v", unchanged, err)
	}
	if _, err := Sync(ctx, database, root, true); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetPolicy(ctx, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrackChannel != model.TrackExact || got.Constraint != "2.4.1" || got.ExpectedIntegrity != integrity {
		t.Fatalf("lock was not consumed: %#v", got)
	}
	if got.ApplyMode != model.ApplyManual || got.LocalCapMode != model.ApplyIgnore || got.NotifyMode != model.NotifyNone {
		t.Fatalf("lock elevated local policy: %#v", got)
	}
}

func TestBindingIntegrityDoesNotTreatLongVersionAsGitCommit(t *testing.T) {
	ctx := context.Background()
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	source := model.Source{ID: "source", Kind: model.SourceNPM, Locator: "example", PackageName: "example", IdentityHash: strings.Repeat("1", 64), MetadataJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	_ = database.UpsertSource(ctx, source)
	component := model.LogicalComponent{ID: "component", Kind: model.ComponentCLI, Name: "example", SourceID: source.ID, LogicalKey: "key", CreatedAt: now, UpdatedAt: now}
	_ = database.UpsertComponent(ctx, component)
	binding := model.Binding{ID: "binding", ComponentID: component.ID, Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/tmp/example", Managed: true, Classification: model.ClassificationClean, LastSeenAt: now}
	_ = database.UpsertBinding(ctx, binding)
	tree := strings.Repeat("2", 64)
	if err := database.PutObjectRecord(ctx, model.ObjectRecord{Hash: tree, Kind: model.ObjectTree, RelativePath: tree, VerifiedAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	resolved := "release-version-with-many-characters-123456"
	if err := database.PutGeneration(ctx, model.Generation{ID: "generation", BindingID: binding.ID, ResolvedRef: resolved, TreeHash: tree, State: model.GenerationActive, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetActiveGeneration(ctx, binding.ID, "generation"); err != nil {
		t.Fatal(err)
	}
	binding, _ = database.GetBinding(ctx, binding.ID)
	_, version, commit := bindingIntegrity(ctx, database, binding)
	if version != resolved || commit != "" {
		t.Fatalf("version=%q commit=%q", version, commit)
	}
}
