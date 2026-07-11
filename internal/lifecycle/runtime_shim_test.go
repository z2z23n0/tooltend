package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/adapter"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/model"
)

func TestCLIRuntimeAdoptionRejectsShadowedShim(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentCLI, adapter.SourceNPM)
	earlier := filepath.Join(t.TempDir(), "earlier-bin")
	if err := os.MkdirAll(earlier, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture.installPath = filepath.Join(earlier, "component")
	if err := os.WriteFile(fixture.installPath, []byte("#!/bin/sh\necho native\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", earlier+string(os.PathListSeparator)+fixture.paths.ShimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fixture.seedBindingOnly(t, false, model.ApplyManual)

	_, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
		Source: "npm:component", Version: "1.0.0", Executable: "component",
	})
	if err == nil || !strings.Contains(err.Error(), "shadowed") {
		t.Fatalf("adoption error=%v", err)
	}
	binding, err := fixture.database.GetBinding(context.Background(), "binding")
	if err != nil || binding.Managed {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.paths.ShimDir, "component")); !os.IsNotExist(err) {
		t.Fatalf("shadowed adoption wrote shim: %v", err)
	}
}

func TestStdioBindingsUseIndependentRuntimeShims(t *testing.T) {
	fixture := newLifecycleFixture(t, model.ComponentStdioMCP, adapter.SourceNPM)
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	if err := fixture.database.UpsertComponent(ctx, model.LogicalComponent{
		ID: "component", Kind: model.ComponentStdioMCP, Name: "component", LogicalKey: "test:component",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	codexConfig := filepath.Join(fixture.paths.ConfigDir, "codex.toml")
	if err := os.WriteFile(codexConfig, []byte("[mcp_servers.component]\ncommand = \"npx\"\nargs = [\"-y\", \"component@1.0.0\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeConfig := filepath.Join(fixture.paths.ConfigDir, "claude.json")
	claudeDocument := map[string]any{"mcpServers": map[string]any{"component": map[string]any{
		"command": "npx", "args": []any{"-y", "component@1.0.0"},
	}}}
	payload, _ := json.Marshal(claudeDocument)
	if err := os.WriteFile(claudeConfig, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	bindings := []model.Binding{
		{ID: "binding-codex", ComponentID: "component", Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: codexConfig + "#mcp_servers/component", ConfigPath: codexConfig, ConfigPointer: "mcp_servers/component", InstallMethod: "npx", ObservedVersion: "1.0.0", Classification: model.ClassificationClean, LastSeenAt: now},
		{ID: "binding-claude", ComponentID: "component", Host: model.HostClaude, Scope: model.ScopeGlobal, InstallPath: claudeConfig + "#mcpServers/component", ConfigPath: claudeConfig, ConfigPointer: "mcpServers/component", InstallMethod: "npx", ObservedVersion: "1.0.0", Classification: model.ClassificationClean, LastSeenAt: now},
	}
	for _, binding := range bindings {
		if err := fixture.database.UpsertBinding(ctx, binding); err != nil {
			t.Fatal(err)
		}
		policy := model.DefaultPolicy()
		policy.BindingID, policy.ApplyMode, policy.LocalCapMode, policy.UpdatedAt = binding.ID, model.ApplyAuto, model.ApplyAuto, now
		if err := fixture.database.SetPolicy(ctx, policy); err != nil {
			t.Fatal(err)
		}
	}
	fixture.provider.latest = "1.0.0"
	first, err := fixture.service.Adopt(ctx, "component", AdoptOptions{BindingID: bindings[0].ID, Source: "npm:component", Version: "1.0.0", Executable: "component"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.Adopt(ctx, "component", AdoptOptions{BindingID: bindings[1].ID, Source: "npm:component", Version: "1.0.0", Executable: "component"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Shim == second.Shim || filepath.Base(first.Shim) == "component" || filepath.Base(second.Shim) == "component" {
		t.Fatalf("shims are not binding-specific: %q %q", first.Shim, second.Shim)
	}
	for index, config := range []struct {
		path    string
		pointer string
		shim    string
	}{{codexConfig, "mcp_servers/component", first.Shim}, {claudeConfig, "mcpServers/component", second.Shim}} {
		command, err := host.CommandAtPointer(config.path, config.pointer)
		if err != nil || command != config.shim {
			t.Fatalf("config %d command=%q err=%v", index, command, err)
		}
	}

	fixture.provider.latest = "2.0.0"
	for _, binding := range bindings {
		updated, err := fixture.service.Update(ctx, "component", binding.ID, UpdateOptions{})
		if err != nil || !updated.Activated {
			t.Fatalf("update %s=%+v err=%v", binding.ID, updated, err)
		}
	}
	for index, binding := range bindings {
		target := first.Generation
		if index == 1 {
			target = second.Generation
		}
		rolledBack, err := fixture.service.Rollback(ctx, "component", binding.ID, target)
		if err != nil || rolledBack.To != target {
			t.Fatalf("rollback %s=%+v err=%v", binding.ID, rolledBack, err)
		}
	}
}

func TestStdioRuntimeReceivesOnlyServerArguments(t *testing.T) {
	tests := []struct {
		name, command, source string
		kind                  adapter.SourceKind
		args                  []string
	}{
		{"npx", "npx", "npm:component", adapter.SourceNPM, []string{"-y", "component@1.0.0", "--transport", "stdio"}},
		{"uvx", "uvx", "pypi:component", adapter.SourcePyPI, []string{"--offline", "component==1.0.0", "--transport", "stdio"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLifecycleFixture(t, model.ComponentStdioMCP, test.kind)
			provider := &argvRuntimeAdapter{kind: test.kind}
			registry, err := adapter.NewRegistry(provider)
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.Adapters = registry
			configPath := filepath.Join(fixture.paths.ConfigDir, test.name+".toml")
			document := map[string]any{"mcp_servers": map[string]any{"component": map[string]any{
				"command": test.command, "args": test.args, "env": map[string]any{"API_TOKEN": "secret-value"},
			}}}
			payload, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			// JSON syntax is also valid input only with a .json extension.
			configPath = strings.TrimSuffix(configPath, ".toml") + ".json"
			if err := os.WriteFile(configPath, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			fixture.installPath = configPath + "#mcp_servers/component"
			fixture.seedBindingOnly(t, false, model.ApplyManual)
			binding, err := fixture.database.GetBinding(context.Background(), "binding")
			if err != nil {
				t.Fatal(err)
			}
			binding.ConfigPath, binding.ConfigPointer = configPath, "mcp_servers/component"
			binding.ObservedVersion = "1.0.0"
			if err := fixture.database.UpsertBinding(context.Background(), binding); err != nil {
				t.Fatal(err)
			}
			adopted, err := fixture.service.Adopt(context.Background(), "component", AdoptOptions{
				Source: test.source, Version: "1.0.0", Executable: "component",
			})
			if err != nil {
				t.Fatal(err)
			}
			spec, err := host.CommandSpecAtPointer(configPath, "mcp_servers/component")
			if err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(spec.Command, spec.Args...).CombinedOutput()
			if err != nil {
				t.Fatalf("run %s: %v: %s", adopted.Shim, err, output)
			}
			if strings.TrimSpace(string(output)) != "--transport\nstdio" || strings.Contains(string(output), "component") || strings.Contains(string(output), "-y") {
				t.Fatalf("managed argv=%q spec=%+v", output, spec)
			}
			content, _ := os.ReadFile(configPath)
			if !strings.Contains(string(content), "secret-value") {
				t.Fatalf("secret reference was lost: %s", content)
			}
		})
	}
}

type argvRuntimeAdapter struct {
	kind adapter.SourceKind
}

func (a *argvRuntimeAdapter) Name() string                { return "argv-runtime" }
func (a *argvRuntimeAdapter) Kinds() []adapter.SourceKind { return []adapter.SourceKind{a.kind} }
func (a *argvRuntimeAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Check: true, Stage: true, ManagedAuto: true, Rollback: true}
}
func (a *argvRuntimeAdapter) Normalize(source adapter.Source) (adapter.Source, error) {
	if a.kind == adapter.SourceNPM {
		return (adapter.NPM{}).Normalize(source)
	}
	return (adapter.Python{}).Normalize(source)
}
func (a *argvRuntimeAdapter) Resolve(_ context.Context, source adapter.Source, track adapter.Track) (adapter.Resolved, error) {
	if track.Constraint == "" {
		return adapter.Resolved{}, errors.New("exact version required")
	}
	separator := "@"
	if a.kind == adapter.SourcePyPI {
		separator = "=="
	}
	return adapter.Resolved{Version: track.Constraint, Ref: source.PackageName + separator + track.Constraint}, nil
}
func (a *argvRuntimeAdapter) Fetch(_ context.Context, _ adapter.Source, resolved adapter.Resolved, staging string) (adapter.Artifact, error) {
	if err := os.RemoveAll(staging); err != nil {
		return adapter.Artifact{}, err
	}
	bin := filepath.Join(staging, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return adapter.Artifact{}, err
	}
	if err := os.WriteFile(filepath.Join(bin, "component"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		return adapter.Artifact{}, err
	}
	return adapter.Artifact{Root: staging, Executable: bin, Integrity: resolved.Ref}, nil
}
func (a *argvRuntimeAdapter) Verify(_ context.Context, _ adapter.Source, resolved adapter.Resolved, artifact adapter.Artifact) error {
	if artifact.Integrity != resolved.Ref {
		return errors.New("integrity mismatch")
	}
	return nil
}
