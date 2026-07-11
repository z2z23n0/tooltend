package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestGitMonorepoSubdirAdoptThenUpdate(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	fixture.provider.expectedSubdir = "plugins/demo"
	if err := os.MkdirAll(fixture.installPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.installPath, "plugin.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := inventory.Report{HostResult: host.Result{
		Observations: []host.Observation{{
			Key: "monorepo-plugin", Host: host.Codex, Kind: host.ComponentPlugin, Name: "component",
			Path: fixture.installPath, Scope: host.ScopeUser, Version: "v1",
			Source: host.SourceRef{
				Kind: "git", Locator: "https://example.test/monorepo.git?ignored=query#ignored-fragment",
				Package: "component", Subdir: "./plugins/demo", Ref: "v1",
			},
		}},
		Bindings: []host.Binding{{
			Host: host.Codex, ComponentKey: "monorepo-plugin", Scope: host.ScopeUser, InstallPath: fixture.installPath,
		}},
	}}
	if _, err := inventory.Persist(context.Background(), fixture.database, report); err != nil {
		t.Fatal(err)
	}
	components, err := fixture.database.ListComponents(context.Background())
	if err != nil || len(components) != 1 {
		t.Fatalf("components=%#v err=%v", components, err)
	}
	componentID := components[0].ID
	originalSourceID := components[0].SourceID

	options := AdoptOptions{
		Source: "https://example.test/monorepo.git?ignored=query#ignored-fragment",
		Subdir: "./plugins/demo", Version: "v1",
	}
	target, err := fixture.service.ResolveAdopt(context.Background(), componentID, options)
	if err != nil {
		t.Fatal(err)
	}
	if target.Subdir != "plugins/demo" || target.SourceIdentity == "" {
		t.Fatalf("adopt target=%#v", target)
	}
	options.ExpectedSourceIdentity = target.SourceIdentity
	options.ExpectedResolvedRef = target.ResolvedRef
	options.ExpectedObservedHash = target.ObservedHash
	adopted, err := fixture.service.Adopt(context.Background(), componentID, options)
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Baseline {
		t.Fatalf("adopted=%#v", adopted)
	}
	component, _, err := fixture.service.Component(context.Background(), componentID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := fixture.database.GetSource(context.Background(), component.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Locator != "https://example.test/monorepo" || source.Subdir != "plugins/demo" {
		t.Fatalf("managed source=%#v", source)
	}
	if component.SourceID != originalSourceID {
		t.Fatalf("inventory source was replaced: before=%s after=%s", originalSourceID, component.SourceID)
	}

	fixture.provider.latest = "v2"
	updated, err := fixture.service.Update(context.Background(), componentID, "", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Activated || updated.Candidate.Status != model.CandidateActive {
		t.Fatalf("updated=%#v", updated)
	}
	if len(fixture.provider.fetchSubdirs) != 2 || fixture.provider.fetchSubdirs[0] != "plugins/demo" || fixture.provider.fetchSubdirs[1] != "plugins/demo" {
		t.Fatalf("fetch subdirs=%#v", fixture.provider.fetchSubdirs)
	}
	content, err := os.ReadFile(filepath.Join(fixture.installPath, "plugin.txt"))
	if err != nil || string(content) != "v2\n" {
		t.Fatalf("active subdir content=%q err=%v", content, err)
	}
}

func TestAdoptSubdirValidationAndConfirmationBinding(t *testing.T) {
	gitFixture := newLifecycleFixture(t, model.ComponentPlugin, adapter.SourceGit)
	if err := os.MkdirAll(gitFixture.installPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitFixture.installPath, "plugin.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitFixture.seedBindingOnly(t, false, model.ApplyManual)
	if _, err := gitFixture.service.ResolveAdopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/monorepo", Subdir: "../../escape",
	}); err == nil || !strings.Contains(err.Error(), "subdirectory") {
		t.Fatalf("escaping subdir accepted: %v", err)
	}
	target, err := gitFixture.service.ResolveAdopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/monorepo", Subdir: "plugins/one",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = gitFixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "https://example.test/monorepo", Subdir: "plugins/two",
		ExpectedSourceIdentity: target.SourceIdentity, ExpectedResolvedRef: target.ResolvedRef,
		ExpectedObservedHash: target.ObservedHash,
	})
	if err == nil || !strings.Contains(err.Error(), "source identity changed") {
		t.Fatalf("changed confirmed subdir accepted: %v", err)
	}

	runtimeFixture := newLifecycleFixture(t, model.ComponentCLI, adapter.SourceNPM)
	if err := os.MkdirAll(filepath.Dir(runtimeFixture.installPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeFixture.installPath, []byte("native\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeFixture.seedBindingOnly(t, false, model.ApplyManual)
	if _, err := runtimeFixture.service.ResolveAdopt(context.Background(), "component", AdoptOptions{
		Source: "npm:component", Subdir: "packages/component", Version: "1.0.0",
	}); err == nil || !strings.Contains(err.Error(), "only for Git") {
		t.Fatalf("non-Git subdir accepted: %v", err)
	}
}
