package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/lifecycle"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestExplicitSkillDependencyDiscoveryPersistsLocatedRuntimeBindingAndEnqueuesMigration(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "project")
	skillFile := filepath.Join(project, ".agents", "skills", "reporter", "SKILL.md")
	binDir := filepath.Join(t.TempDir(), "bin")
	executable := filepath.Join(binDir, "report-cli")
	for _, directory := range []string{filepath.Join(project, ".git"), filepath.Dir(skillFile), binDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(skillFile, []byte("---\nname: reporter\n---\n\n```sh\nnpx -y @scope/report-cli@2.1.0 render\nuvx missing-report==1.0.0\nnpx --version\nnpx --api-key must-not-become-a-package actual-package\n```\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	discovered, err := host.NewCodex().Scan(ctx, host.ScanOptions{
		HomeDir: home, Projects: []string{project}, CodexSystemDir: filepath.Join(home, "missing-system"),
		Getenv: func(name string) string {
			if name == "PATH" {
				return binDir
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var located host.DependencyRef
	for _, observation := range discovered.Observations {
		if observation.Name != "reporter" {
			continue
		}
		for _, dependency := range observation.Dependencies {
			if dependency.PackageIdentity == "npm:@scope/report-cli" {
				located = dependency
			}
		}
	}
	if located.InstallPath != executable || located.Executable != "report-cli" || located.Carrier != "npx" {
		t.Fatalf("located dependency=%#v", located)
	}
	if located.InstallPath == skillFile {
		t.Fatal("dependency evidence path was reused as the executable install path")
	}

	database, paths := openWorkerStore(t)
	persisted, err := inventory.Persist(ctx, database, inventory.Report{HostResult: discovered, Projects: []string{project}})
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Dependencies != 2 || persisted.Bindings != 2 {
		t.Fatalf("persisted=%#v", persisted)
	}
	components, err := database.ListComponents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cli model.LogicalComponent
	var unlocated model.LogicalComponent
	for _, component := range components {
		if component.Kind == model.ComponentCLI && component.Name == "@scope/report-cli" {
			cli = component
		}
		if component.Kind == model.ComponentCLI && component.Name == "missing-report" {
			unlocated = component
		}
		if component.Name == "npx" || component.Name == "uvx" {
			t.Fatalf("carrier became a component: %#v", component)
		}
	}
	if cli.ID == "" {
		t.Fatalf("located CLI component missing: %#v", components)
	}
	if unlocated.ID == "" {
		t.Fatalf("unlocated package dependency edge target missing: %#v", components)
	}
	unlocatedBindings, err := database.ListBindings(ctx, unlocated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unlocatedBindings) != 0 {
		t.Fatalf("temporary uvx package without a local executable gained a binding: %#v", unlocatedBindings)
	}
	bindings, err := database.ListBindings(ctx, cli.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("CLI bindings=%#v", bindings)
	}
	binding := bindings[0]
	if binding.Host != model.HostCodex || binding.Scope != model.ScopeProject || binding.ProjectID == "" ||
		binding.InstallPath != executable || binding.InstallMethod != "observed-runtime:npx" || binding.ObservedVersion != "2.1.0" {
		t.Fatalf("CLI provenance/path not preserved: %#v", binding)
	}
	policy, err := database.GetPolicy(ctx, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ApplyMode != model.ApplyManual || policy.LocalCapMode != model.ApplyManual {
		t.Fatalf("unmanaged dependency was not fail-closed: %#v", policy)
	}
	queued, err := EnqueueRuntimeMigrations(ctx, database, time.Date(2026, 7, 12, 6, 0, 0, 0, time.UTC))
	if err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	var taskBinding string
	if err := database.DB().QueryRowContext(ctx, `SELECT binding_id FROM tasks WHERE kind='adopt_runtime_auto'`).Scan(&taskBinding); err != nil {
		t.Fatal(err)
	}
	if taskBinding != binding.ID {
		t.Fatalf("migration task binding=%q want %q", taskBinding, binding.ID)
	}

	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", paths.ShimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		t.Fatal(err)
	}
	service, err := lifecycle.New(database, objects, paths)
	if err != nil {
		t.Fatal(err)
	}
	service.Adapters, err = adapter.NewRegistry(dependencyRuntimeAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := database.GetSource(ctx, cli.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceBefore.Locator != "https://registry.npmjs.org/@scope/report-cli" || sourceBefore.PackageName != "@scope/report-cli" {
		t.Fatalf("inventory source was not adapter-canonical: %#v", sourceBefore)
	}
	options := lifecycle.AdoptOptions{
		Source: "npm:@scope/report-cli", Version: "2.1.0", BindingID: binding.ID, Executable: "report-cli",
	}
	target, err := service.ResolveAdopt(ctx, cli.ID, options)
	if err != nil {
		t.Fatalf("resolve discovered dependency adoption: %v", err)
	}
	options.ExpectedResolvedRef, options.ExpectedObservedHash = target.ResolvedRef, target.ObservedHash
	adopted, err := service.Adopt(ctx, cli.ID, options)
	if err != nil {
		t.Fatalf("adopt discovered dependency runtime: %v", err)
	}
	if adopted.BindingID != binding.ID || adopted.Shim == "" {
		t.Fatalf("adopted=%#v", adopted)
	}
	managedComponent, managedBindings, err := service.Component(ctx, cli.ID)
	if err != nil || len(managedBindings) != 1 || !managedBindings[0].Managed {
		t.Fatalf("managed component=%#v bindings=%#v err=%v", managedComponent, managedBindings, err)
	}
	if managedComponent.SourceID != sourceBefore.ID {
		t.Fatalf("canonical inventory source was replaced: before=%s after=%s", sourceBefore.ID, managedComponent.SourceID)
	}
}

type dependencyRuntimeAdapter struct{}

func (dependencyRuntimeAdapter) Name() string { return "dependency-runtime-test" }
func (dependencyRuntimeAdapter) Kinds() []adapter.SourceKind {
	return []adapter.SourceKind{adapter.SourceNPM}
}
func (dependencyRuntimeAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Check: true, Stage: true, ManagedAuto: true, Rollback: true}
}
func (dependencyRuntimeAdapter) Normalize(source adapter.Source) (adapter.Source, error) {
	return adapter.CanonicalizeSource(adapter.SourceNPM, source)
}
func (dependencyRuntimeAdapter) Resolve(_ context.Context, source adapter.Source, track adapter.Track) (adapter.Resolved, error) {
	return adapter.Resolved{Version: track.Constraint, Ref: source.PackageName + "@" + track.Constraint}, nil
}
func (dependencyRuntimeAdapter) Fetch(_ context.Context, _ adapter.Source, resolved adapter.Resolved, stagingDir string) (adapter.Artifact, error) {
	bin := filepath.Join(stagingDir, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return adapter.Artifact{}, err
	}
	if err := os.WriteFile(filepath.Join(bin, "report-cli"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		return adapter.Artifact{}, err
	}
	return adapter.Artifact{Root: stagingDir, Executable: bin, Integrity: resolved.Ref}, nil
}
func (dependencyRuntimeAdapter) Verify(context.Context, adapter.Source, adapter.Resolved, adapter.Artifact) error {
	return nil
}

func TestConfirmedRuntimeMigrationIsExactIdempotentAndPromotesAfterAdopt(t *testing.T) {
	ctx := context.Background()
	database, paths := openWorkerStore(t)
	now := time.Date(2026, 7, 12, 4, 0, 0, 0, time.UTC)
	binding := seedRuntimeBinding(t, database, "runtime", "1.2.3", now)
	queued, err := EnqueueRuntimeMigrations(ctx, database, now)
	if err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	queued, err = EnqueueRuntimeMigrations(ctx, database, now)
	if err != nil || queued != 0 {
		t.Fatalf("duplicate queued=%d err=%v", queued, err)
	}

	adoptions := 0
	worker := Worker{
		Database: database, Paths: paths, Config: config.Default(), Now: func() time.Time { return now },
		Recover: func(context.Context, *store.Store, config.Paths) (int, error) { return 0, nil },
		Inventory: func(context.Context, *store.Store, InventoryOptions) (inventory.PersistResult, error) {
			return inventory.PersistResult{}, nil
		},
		RuntimeAdopter: RuntimeAdopterFunc(func(_ context.Context, current model.Binding) (Outcome, error) {
			adoptions++
			current.Managed = true
			current.InstallMethod = "tooltend-runtime:bin/server"
			if err := database.UpsertBinding(ctx, current); err != nil {
				return Outcome{}, err
			}
			return Outcome{Changed: true, Activated: true, ResolvedRef: "server@1.2.3"}, nil
		}),
		Coordinator: CoordinatorFunc(func(_ context.Context, request Request) (Outcome, error) {
			if !request.Binding.Managed || !request.Stage || !request.Activate {
				t.Fatalf("post-migration check was not automatic: %#v", request)
			}
			return Outcome{Checked: true}, nil
		}),
	}
	result, err := worker.RunOnce(ctx, ReasonKick)
	if err != nil {
		t.Fatal(err)
	}
	if adoptions != 1 || result.Failed != 0 || result.Succeeded != 2 {
		t.Fatalf("adoptions=%d result=%#v", adoptions, result)
	}
	policy, err := database.GetPolicy(ctx, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ApplyMode != model.ApplyAuto || policy.LocalCapMode != model.ApplyAuto {
		t.Fatalf("confirmed migration did not enable safe automatic checks: %#v", policy)
	}
}

func TestRuntimeMigrationSkipsMovingOrUnlocatedRuntime(t *testing.T) {
	ctx := context.Background()
	database, _ := openWorkerStore(t)
	now := time.Now().UTC()
	seedRuntimeBinding(t, database, "moving", "latest", now)
	binding := seedRuntimeBinding(t, database, "unlocated", "2.0.0", now)
	binding.ConfigPointer = ""
	if err := database.UpsertBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	queued, err := EnqueueRuntimeMigrations(ctx, database, now)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("unsafe runtime migrations queued=%d", queued)
	}
}

func seedRuntimeBinding(t *testing.T, database *store.Store, id, version string, now time.Time) model.Binding {
	t.Helper()
	ctx := context.Background()
	source := model.Source{ID: "src_" + id, Kind: model.SourceNPM, Locator: "server", PackageName: "server", IdentityHash: digestForTest("source-" + id), MetadataJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{ID: "cmp_" + id, Kind: model.ComponentStdioMCP, Name: id, SourceID: source.ID, LogicalKey: digestForTest("component-" + id), CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	binding := model.Binding{
		ID: id, ComponentID: component.ID, Host: model.HostCodex, Scope: model.ScopeGlobal,
		InstallPath: configPath + "#mcp_servers/" + id, ConfigPath: configPath, ConfigPointer: "mcp_servers/" + id,
		InstallMethod: "observed-runtime:npx", ObservedVersion: version, Classification: model.ClassificationClean, LastSeenAt: now,
	}
	if err := database.UpsertBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := database.SetPolicy(ctx, model.Policy{BindingID: id, TrackChannel: model.TrackStable, ApplyMode: model.ApplyManual, NotifyMode: model.NotifyFailures, LocalCapMode: model.ApplyManual, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return binding
}
