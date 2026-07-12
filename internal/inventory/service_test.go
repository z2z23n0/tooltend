package inventory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestPersistClassifiesInitialObservations(t *testing.T) {
	database := openTestStore(t)
	observations := []host.Observation{
		{Key: "local", Host: host.Codex, Kind: host.ComponentSkill, Name: "local", Path: "/tmp/local", Source: host.SourceRef{Kind: "local", Locator: "/tmp/local"}},
		{Key: "unknown", Host: host.Codex, Kind: host.ComponentSkill, Name: "unknown", Path: "/tmp/unknown", Source: host.SourceRef{}},
		{Key: "remote", Host: host.Codex, Kind: host.ComponentHTTPMCP, Name: "remote", Source: host.SourceRef{Kind: "http", Locator: "https://example.test/mcp"}},
		{Key: "git", Host: host.Codex, Kind: host.ComponentPlugin, Name: "git", Source: host.SourceRef{Kind: "git", Locator: "https://github.com/example/plugin.git", Ref: "v1.2.3"}},
	}
	bindings := make([]host.Binding, 0, len(observations))
	for _, observation := range observations {
		bindings = append(bindings, host.Binding{Host: host.Codex, ComponentKey: observation.Key, Scope: host.ScopeUser, InstallPath: "/install/" + observation.Key})
	}
	if _, err := Persist(context.Background(), database, Report{HostResult: host.Result{Observations: observations, Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]model.Classification{
		"/install/local": model.ClassificationDetached, "/install/unknown": model.ClassificationUnknown,
		"/install/remote": model.ClassificationClean, "/install/git": model.ClassificationClean,
	}
	got, err := database.ListBindings(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range got {
		if binding.Classification != want[binding.InstallPath] {
			t.Errorf("%s classification = %s, want %s", binding.InstallPath, binding.Classification, want[binding.InstallPath])
		}
		policy, policyErr := database.GetPolicy(context.Background(), binding.ID)
		if policyErr != nil {
			t.Fatal(policyErr)
		}
		wantMode := model.ApplyManual
		if binding.InstallPath == "/install/git" {
			wantMode = model.ApplyAuto
		}
		if policy.ApplyMode != wantMode {
			t.Errorf("%s apply mode = %s, want %s", binding.InstallPath, policy.ApplyMode, wantMode)
		}
	}
}

func TestPersistForcesHostOwnedBindingsAndDependenciesToIgnore(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	executable := filepath.Join(t.TempDir(), "bin", "plugin-cli")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	observation := host.Observation{
		Key: "plugin-skill", Host: host.Codex, Kind: host.ComponentSkill, Name: "plugin-skill",
		Path: "/cache/plugin/skills/plugin-skill", Scope: host.ScopePlugin,
		Source:   host.SourceRef{Kind: "local", Locator: "/cache/plugin/skills/plugin-skill"},
		Metadata: map[string]string{"lifecycle_owner": string(host.Codex)},
		Dependencies: []host.DependencyRef{{
			PackageIdentity: "npm:plugin-cli", Constraint: "1.2.3",
			Source:     host.SourceRef{Kind: "npm", Package: "plugin-cli", Version: "1.2.3"},
			Executable: "plugin-cli", InstallPath: executable, Carrier: "npx",
			EvidencePath: "/cache/plugin/skills/plugin-skill/SKILL.md", EvidenceLine: 8,
		}},
	}
	report := Report{HostResult: host.Result{
		Observations: []host.Observation{observation},
		Bindings:     []host.Binding{{Host: host.Codex, ComponentKey: observation.Key, Scope: host.ScopePlugin, InstallPath: observation.Path}},
	}}
	if _, err := Persist(ctx, database, report); err != nil {
		t.Fatal(err)
	}
	bindings, err := database.ListBindings(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %#v", bindings)
	}
	for _, binding := range bindings {
		if binding.InstallMethod != "host-owned:codex" {
			t.Errorf("%s install method = %q", binding.InstallPath, binding.InstallMethod)
		}
		policy, policyErr := database.GetPolicy(ctx, binding.ID)
		if policyErr != nil {
			t.Fatal(policyErr)
		}
		if policy.ApplyMode != model.ApplyIgnore || policy.LocalCapMode != model.ApplyIgnore {
			t.Errorf("%s policy = %#v", binding.InstallPath, policy)
		}
		policy.ApplyMode, policy.LocalCapMode, policy.UpdatedAt = model.ApplyAuto, model.ApplyAuto, time.Now().UTC()
		if err := database.SetPolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Persist(ctx, database, report); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		policy, err := database.GetPolicy(ctx, binding.ID)
		if err != nil {
			t.Fatal(err)
		}
		if policy.ApplyMode != model.ApplyIgnore || policy.LocalCapMode != model.ApplyIgnore {
			t.Errorf("host-owned policy was loosened: %#v", policy)
		}
	}
}

func TestPersistPreservesManagedBindingAndPolicy(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	now := time.Now().UTC()
	source := model.Source{ID: "src_adopted", Kind: model.SourceGit, Locator: "https://github.com/example/skill", IdentityHash: digest("adopted"), MetadataJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertSource(ctx, source); err != nil {
		t.Fatal(err)
	}
	component := model.LogicalComponent{ID: "cmp_adopted", Kind: model.ComponentSkill, Name: "skill", SourceID: source.ID, LogicalKey: digest("component"), CreatedAt: now, UpdatedAt: now}
	if err := database.UpsertComponent(ctx, component); err != nil {
		t.Fatal(err)
	}
	installPath := "/managed/skill"
	bindingID := stableID("bnd", digest(string(host.Codex)+"\x00"+installPath))
	original := model.Binding{
		ID: bindingID, ComponentID: component.ID, Host: model.HostCodex, Scope: model.ScopeGlobal,
		InstallPath: installPath, InstallMethod: "generation_symlink", Managed: true,
		Classification: model.ClassificationCustomized, ObservedHash: digest("observed"), ObservedVersion: "1.2.3",
		TrustHash: digest("trust"), LastSeenAt: now,
	}
	if err := database.UpsertBinding(ctx, original); err != nil {
		t.Fatal(err)
	}
	treeHash := digest("tree")
	if err := database.PutObjectRecord(ctx, model.ObjectRecord{Hash: treeHash, Kind: model.ObjectTree, RelativePath: treeHash, VerifiedAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.PutGeneration(ctx, model.Generation{ID: "gen_active", BindingID: bindingID, ResolvedRef: "1.2.3", TreeHash: treeHash, State: model.GenerationActive, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := database.SetActiveGeneration(ctx, bindingID, "gen_active"); err != nil {
		t.Fatal(err)
	}
	original.ActiveGenerationID = "gen_active"
	policy := model.Policy{BindingID: bindingID, TrackChannel: model.TrackExact, Constraint: "1.2.3", ApplyMode: model.ApplyIgnore, NotifyMode: model.NotifyNone, LocalCapMode: model.ApplyIgnore, UpdatedAt: now}
	if err := database.SetPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}

	report := Report{HostResult: host.Result{
		Observations: []host.Observation{{Key: "local-scan", Host: host.Codex, Kind: host.ComponentSkill, Name: "skill", Path: installPath, Source: host.SourceRef{Kind: "local", Locator: installPath}}},
		Bindings:     []host.Binding{{Host: host.Codex, ComponentKey: "local-scan", Scope: host.ScopeUser, InstallPath: installPath}},
	}}
	if _, err := Persist(ctx, database, report); err != nil {
		t.Fatal(err)
	}

	components, err := database.ListComponents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 1 || components[0].ID != component.ID {
		t.Fatalf("managed symlink created a local component: %#v", components)
	}
	got, err := database.GetBinding(ctx, bindingID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ComponentID != original.ComponentID || got.InstallMethod != original.InstallMethod || !got.Managed ||
		got.Classification != original.Classification || got.ObservedHash != original.ObservedHash ||
		got.ObservedVersion != original.ObservedVersion || got.TrustHash != original.TrustHash {
		t.Fatalf("managed state changed: %#v", got)
	}
	if got.ActiveGenerationID != original.ActiveGenerationID {
		t.Fatalf("active generation changed: %q", got.ActiveGenerationID)
	}
	gotPolicy, err := database.GetPolicy(ctx, bindingID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPolicy.TrackChannel != model.TrackExact || gotPolicy.ApplyMode != model.ApplyIgnore || gotPolicy.LocalCapMode != model.ApplyIgnore {
		t.Fatalf("policy was overwritten: %#v", gotPolicy)
	}
}

func TestConfigBackedComponentsUsePointerScopedBindingIdentity(t *testing.T) {
	database := openTestStore(t)
	configPath := "/tmp/.codex/config.toml"
	observations := []host.Observation{
		{Key: "mcp-a", Host: host.Codex, Kind: host.ComponentStdioMCP, Name: "a", Path: configPath, Scope: host.ScopeUser, Source: host.SourceRef{Kind: "npm", Package: "server-a", Version: "1.0.0"}, Evidence: []host.Evidence{{Path: configPath, Pointer: "mcp_servers/a"}}},
		{Key: "mcp-b", Host: host.Codex, Kind: host.ComponentStdioMCP, Name: "b", Path: configPath, Scope: host.ScopeUser, Source: host.SourceRef{Kind: "npm", Package: "server-b", Version: "2.0.0"}, Evidence: []host.Evidence{{Path: configPath, Pointer: "mcp_servers/b"}}},
	}
	bindings := []host.Binding{
		{Host: host.Codex, ComponentKey: "mcp-a", Scope: host.ScopeUser, InstallPath: configPath, ConfigPath: configPath},
		{Host: host.Codex, ComponentKey: "mcp-b", Scope: host.ScopeUser, InstallPath: configPath, ConfigPath: configPath},
	}
	if _, err := Persist(context.Background(), database, Report{HostResult: host.Result{Observations: observations, Bindings: bindings}}); err != nil {
		t.Fatal(err)
	}
	values, err := database.ListBindings(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].InstallPath == values[1].InstallPath {
		t.Fatalf("bindings=%#v", values)
	}
	for _, value := range values {
		if value.ConfigPath != configPath || value.ConfigPointer == "" {
			t.Fatalf("config locator missing: %#v", value)
		}
	}
}

func TestPersistCreatesExplicitCLIComponentAndDependencyEdge(t *testing.T) {
	database := openTestStore(t)
	skillPath := "/skills/reporter/SKILL.md"
	observation := host.Observation{
		Key: "skill", Host: host.Codex, Kind: host.ComponentSkill, Name: "reporter", Path: "/skills/reporter",
		Source: host.SourceRef{Kind: "git", Locator: "https://example.test/reporter"},
		Dependencies: []host.DependencyRef{{
			PackageIdentity: "npm:@scope/report-cli", Constraint: "^2.1",
			Source:       host.SourceRef{Kind: "npm", Package: "@scope/report-cli", Version: "^2.1"},
			EvidencePath: skillPath, EvidenceLine: 12,
		}},
	}
	report := Report{HostResult: host.Result{
		Observations: []host.Observation{observation},
		Bindings:     []host.Binding{{Host: host.Codex, ComponentKey: "skill", Scope: host.ScopeUser, InstallPath: "/skills/reporter"}},
	}}
	result, err := Persist(context.Background(), database, report)
	if err != nil {
		t.Fatal(err)
	}
	if result.Dependencies != 1 {
		t.Fatalf("persist result=%+v", result)
	}
	var packageIdentity, constraint, evidencePath string
	var evidenceLine, explicit int
	if err := database.DB().QueryRow(`SELECT package_identity,constraint_text,evidence_path,evidence_line,explicit FROM dependencies`).
		Scan(&packageIdentity, &constraint, &evidencePath, &evidenceLine, &explicit); err != nil {
		t.Fatal(err)
	}
	if packageIdentity != "npm:@scope/report-cli" || constraint != "^2.1" || evidencePath != skillPath || evidenceLine != 12 || explicit != 1 {
		t.Fatalf("dependency=%q %q %q %d %d", packageIdentity, constraint, evidencePath, evidenceLine, explicit)
	}
	components, err := database.ListComponents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundCLI := false
	for _, component := range components {
		foundCLI = foundCLI || component.Kind == model.ComponentCLI && component.Name == "@scope/report-cli"
	}
	if !foundCLI {
		t.Fatalf("dedicated CLI component missing: %#v", components)
	}
}

func TestPersistKeepsManagedDependencyBindingWhenDiscoveryMovesToShim(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	nativePath := filepath.Join(t.TempDir(), "native", "report-cli")
	shimPath := filepath.Join(t.TempDir(), "shim", "report-cli")
	for _, path := range []string{nativePath, shimPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	skillPath := filepath.Join(t.TempDir(), "skills", "reporter")
	dependency := host.DependencyRef{
		PackageIdentity: "npm:report-cli", Constraint: "1.2.3",
		Source:     host.SourceRef{Kind: "npm", Package: "report-cli", Version: "1.2.3"},
		Executable: "report-cli", InstallPath: nativePath, Carrier: "npx",
		EvidencePath: filepath.Join(skillPath, "SKILL.md"), EvidenceLine: 8,
	}
	report := func(value host.DependencyRef) Report {
		return Report{HostResult: host.Result{
			Observations: []host.Observation{{
				Key: "skill", Host: host.Codex, Kind: host.ComponentSkill, Name: "reporter", Path: skillPath,
				Scope: host.ScopeUser, Source: host.SourceRef{Kind: "local", Locator: skillPath}, Dependencies: []host.DependencyRef{value},
			}},
			Bindings: []host.Binding{{Host: host.Codex, ComponentKey: "skill", Scope: host.ScopeUser, InstallPath: skillPath}},
		}}
	}
	if _, err := Persist(ctx, database, report(dependency)); err != nil {
		t.Fatal(err)
	}
	components, err := database.ListComponents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cliID string
	for _, component := range components {
		if component.Kind == model.ComponentCLI {
			cliID = component.ID
		}
	}
	bindings, err := database.ListBindings(ctx, cliID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("bindings=%#v err=%v", bindings, err)
	}
	managed := bindings[0]
	managed.Managed = true
	managed.InstallMethod = "tooltend-runtime:node_modules/.bin/report-cli"
	if err := database.UpsertBinding(ctx, managed); err != nil {
		t.Fatal(err)
	}

	dependency.InstallPath = shimPath
	if _, err := Persist(ctx, database, report(dependency)); err != nil {
		t.Fatal(err)
	}
	bindings, err = database.ListBindings(ctx, cliID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("managed dependency was duplicated: %#v err=%v", bindings, err)
	}
	if !bindings[0].Managed || bindings[0].InstallPath != nativePath || bindings[0].InstallMethod != managed.InstallMethod {
		t.Fatalf("managed dependency changed after shim discovery: %#v", bindings[0])
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}
