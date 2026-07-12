package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanAllDiscoversSelectedHostConfigurationWithoutSecretValues(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "selected")
	otherProject := filepath.Join(t.TempDir(), "not-selected")
	mustMkdirAll(t, filepath.Join(project, ".git"))
	mustMkdirAll(t, filepath.Join(otherProject, ".git"))

	mustWriteFile(t, filepath.Join(home, ".codex", "config.toml"), `
[mcp_servers.npm_server]
command = "npx"
args = ["-y", "@scope/server@1.2.3"]
env = { API_TOKEN = "codex-env-secret" }

[mcp_servers.remote_server]
type = "streamable-http"
url = "https://url-user:url-password@example.test/mcp?token=url-query-secret#fragment"
bearer_token_env_var = "REMOTE_TOKEN"
http_headers = { Authorization = "Bearer static-header-secret", X_Key = "static-x-secret" }
env_http_headers = { X_Dynamic = "DYNAMIC_TOKEN" }

[plugins."configured@openai-bundled"]
enabled = true
`)
	mustWriteFile(t, filepath.Join(home, ".codex", "hooks.json"), `{
  "unknown": {"keep": true},
  "hooks": {
    "PreToolUse": [{
      "matcher": "Bash",
      "hooks": [{
        "type": "command",
        "command": "/private/bin/audit --token raw-hook-secret",
        "env": {"HOOK_TOKEN": "hook-env-secret"}
      }]
    }]
  }
}`)
	mustWriteFile(t, filepath.Join(home, ".agents", "skills", "official-skill", "SKILL.md"), "---\nname: official-skill\ndescription: official\n---\n")
	mustWriteFile(t, filepath.Join(home, ".codex", "skills", "legacy-skill", "SKILL.md"), "---\nname: legacy-skill\n---\n")
	mustWriteFile(t, filepath.Join(project, ".codex", "config.toml"), `
[mcp_servers.project_server]
command = "uvx"
args = ["project-mcp==4.5.6"]
env_vars = ["PROJECT_TOKEN"]
`)

	codexPlugin := filepath.Join(home, ".codex", "plugins", "cache", "community", "codex-plugin", "1.0.0")
	mustWriteFile(t, filepath.Join(codexPlugin, ".codex-plugin", "plugin.json"), `{
  "name": "codex-plugin",
  "version": "1.0.0",
  "repository": "https://repo-user:repo-password@example.test/plugin.git?access=repo-query-secret",
  "skills": "./skills",
  "hooks": "./hooks/hooks.json",
  "mcpServers": "./.mcp.json"
}`)
	mustWriteFile(t, filepath.Join(codexPlugin, "skills", "plugin-skill", "SKILL.md"), "---\nname: codex-plugin-skill\n---\n")
	mustWriteFile(t, filepath.Join(codexPlugin, "hooks", "hooks.json"), `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/bin/audit repo-hook-secret"}]}]}}`)
	mustWriteFile(t, filepath.Join(codexPlugin, ".mcp.json"), `{"plugin_server":{"command":"npx","args":["plugin-mcp@7.8.9"],"env":{"PLUGIN_TOKEN":"plugin-secret"}}}`)

	mustWriteFile(t, filepath.Join(home, ".claude", "settings.json"), `{
  "enabledPlugins": {"claude-plugin@official": true},
  "hooks": {
    "PreToolUse": [{
      "matcher": "Bash",
      "hooks": [{"type":"command","command":"/private/bin/observe --secret claude-hook-secret"}]
    }]
  }
}`)
	mustWriteFile(t, filepath.Join(home, ".claude", "skills", "claude-skill", "SKILL.md"), "---\nname: claude-skill\n---\n")
	mustWriteFile(t, filepath.Join(home, ".claude", "commands", "legacy-command.md"), "---\nname: legacy-command\n---\n")
	mustWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{
			"user_server": map[string]any{"command": "npx", "args": []string{"user-mcp@1.0.0"}, "env": map[string]string{"USER_TOKEN": "user-secret"}},
		},
		"projects": map[string]any{
			project: map[string]any{"mcpServers": map[string]any{
				"selected_server": map[string]any{"url": "https://selected.example.test/mcp?key=selected-query-secret", "type": "http"},
			}},
			otherProject: map[string]any{"mcpServers": map[string]any{
				"unselected-secret-server": map[string]any{"command": "npx", "args": []string{"must-not-be-scanned"}},
			}},
		},
	})
	mustWriteFile(t, filepath.Join(project, ".mcp.json"), `{"mcpServers":{"project_http":{"type":"http","url":"https://project.example.test/mcp?api_key=project-url-secret","headers":{"X-API-Key":"project-header-secret"}}}}`)

	claudePlugin := filepath.Join(home, ".claude", "plugins", "cache", "official", "claude-cache-plugin", "2.0.0")
	mustWriteFile(t, filepath.Join(claudePlugin, ".claude-plugin", "plugin.json"), `{"name":"claude-cache-plugin","version":"2.0.0"}`)
	mustWriteFile(t, filepath.Join(claudePlugin, "skills", "plugin-skill", "SKILL.md"), "---\nname: claude-plugin-skill\n---\n")

	result, err := ScanAll(context.Background(), ScanOptions{
		HomeDir: home, CurrentProject: project,
		CodexSystemDir:   filepath.Join(home, "missing-codex-system"),
		ClaudeManagedDir: filepath.Join(home, "missing-claude-managed"),
		Getenv:           func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("ScanAll() error = %v", err)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal(result) error = %v", err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{
		"codex-env-secret", "url-password", "url-query-secret", "static-header-secret", "static-x-secret",
		"raw-hook-secret", "hook-env-secret", "repo-password", "repo-query-secret", "repo-hook-secret", "plugin-secret",
		"claude-hook-secret", "user-secret", "selected-query-secret", "project-url-secret", "project-header-secret",
		"unselected-secret-server", "must-not-be-scanned",
	} {
		if strings.Contains(serialized, forbidden) {
			t.Errorf("scan result leaked or scanned forbidden value %q", forbidden)
		}
	}
	for _, expected := range []string{"API_TOKEN", "REMOTE_TOKEN", "Authorization", "X_Dynamic<-DYNAMIC_TOKEN", "HOOK_TOKEN", "PROJECT_TOKEN", "USER_TOKEN", "X-API-Key"} {
		if !hasSecretReference(result, expected) {
			t.Errorf("scan result is missing safe secret reference %q", expected)
		}
	}

	assertObservation(t, result, Codex, ComponentStdioMCP, "npm_server")
	assertObservation(t, result, Codex, ComponentHTTPMCP, "remote_server")
	assertObservation(t, result, Codex, ComponentSkill, "official-skill")
	legacy := assertObservation(t, result, Codex, ComponentSkill, "legacy-skill")
	if !legacy.Legacy {
		t.Error("legacy Codex skill was not marked legacy")
	}
	configuredPlugin := assertObservation(t, result, Codex, ComponentPlugin, "configured")
	if configuredPlugin.Metadata["lifecycle_owner"] != string(Codex) {
		t.Fatalf("configured plugin lifecycle owner = %q", configuredPlugin.Metadata["lifecycle_owner"])
	}
	cachePlugin := assertObservation(t, result, Codex, ComponentPlugin, "codex-plugin")
	cacheSkill := assertObservation(t, result, Codex, ComponentSkill, "codex-plugin-skill")
	cacheMCP := assertObservation(t, result, Codex, ComponentStdioMCP, "codex-plugin:plugin_server")
	for _, observation := range []Observation{cachePlugin, cacheSkill, cacheMCP} {
		if observation.Metadata["lifecycle_owner"] != string(Codex) {
			t.Errorf("cache observation %s lifecycle owner = %q", observation.Name, observation.Metadata["lifecycle_owner"])
		}
	}
	assertObservation(t, result, Claude, ComponentStdioMCP, "user_server")
	assertObservation(t, result, Claude, ComponentHTTPMCP, "selected_server")
	assertObservation(t, result, Claude, ComponentHTTPMCP, "project_http")
	assertObservation(t, result, Claude, ComponentSkill, "claude-skill")
	assertObservation(t, result, Claude, ComponentSkill, "legacy-command")
	assertObservation(t, result, Claude, ComponentPlugin, "claude-cache-plugin")
}

func hasSecretReference(result Result, expected string) bool {
	for _, observation := range result.Observations {
		for _, secret := range observation.Secrets {
			if secret.Field == expected || secret.Name == expected {
				return true
			}
		}
	}
	return false
}

func TestClaudeGlobalStateOnlyScansSelectedProjects(t *testing.T) {
	home := t.TempDir()
	selected := filepath.Join(t.TempDir(), "selected")
	unselected := filepath.Join(t.TempDir(), "unselected")
	mustMkdirAll(t, filepath.Join(selected, ".git"))
	mustMkdirAll(t, filepath.Join(unselected, ".git"))
	mustWriteJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"projects": map[string]any{
			selected:   map[string]any{"mcpServers": map[string]any{"selected": map[string]any{"command": "selected-command"}}},
			unselected: map[string]any{"mcpServers": map[string]any{"unselected": map[string]any{"command": "unselected-command"}}},
		},
	})

	result, err := NewClaude().Scan(context.Background(), ScanOptions{
		HomeDir: home, CurrentProject: selected,
		ClaudeManagedDir: filepath.Join(home, "missing-managed"),
		Getenv:           func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertObservation(t, result, Claude, ComponentStdioMCP, "selected")
	for _, observation := range result.Observations {
		if observation.Name == "unselected" {
			t.Fatal("unselected project MCP was scanned")
		}
	}
}

func TestEmptyCurrentProjectDoesNotUseProcessWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	selected := filepath.Join(t.TempDir(), "selected")
	options, err := (ScanOptions{HomeDir: home, Projects: []string{selected}}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	if options.CurrentProject != "" {
		t.Fatalf("empty CurrentProject became %q", options.CurrentProject)
	}
	if len(options.Projects) != 1 || options.Projects[0] != filepath.Clean(selected) {
		t.Fatalf("explicit projects changed: %#v", options.Projects)
	}
}

func TestRuntimeSourceTreatsUnknownFlagArgumentsAsOpaque(t *testing.T) {
	source, _ := runtimeSource("npx", []string{"--api-key", "looks-like-a-package-secret", "actual-package"})
	if source.Package != "" || strings.Contains(source.Locator, "looks-like-a-package-secret") {
		t.Fatalf("runtimeSource() exposed an option value: %#v", source)
	}

	source, _ = runtimeSource("node", []string{"--token", "/private/secret-value", "./server.js"})
	if source.Kind == "local" || strings.Contains(source.Locator, "secret-value") {
		t.Fatalf("runtimeSource() exposed an option value as a path: %#v", source)
	}
}

func TestInvalidConfigurationWarningDoesNotIncludeSourceText(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	mustWriteFile(t, filepath.Join(home, ".codex", "config.toml"), "token = \\\"warning-must-not-leak\\\" invalid")

	result, err := NewCodex().Scan(context.Background(), ScanOptions{
		HomeDir: home, CurrentProject: project,
		CodexSystemDir: filepath.Join(home, "missing-system"),
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("json.Marshal() error = %v", marshalErr)
	}
	if strings.Contains(string(encoded), "warning-must-not-leak") {
		t.Fatal("parser warning included source text")
	}
}

func TestClaudePluginCacheOverrideIsTheCacheDirectory(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	cache := filepath.Join(t.TempDir(), "custom-cache")
	plugin := filepath.Join(cache, "official", "overridden-plugin", "3.0.0")
	mustWriteFile(t, filepath.Join(plugin, ".claude-plugin", "plugin.json"), `{"name":"overridden-plugin","version":"3.0.0"}`)

	result, err := NewClaude().Scan(context.Background(), ScanOptions{
		HomeDir: home, CurrentProject: project,
		ClaudeManagedDir: filepath.Join(home, "missing-managed"),
		Getenv: func(name string) string {
			if name == "CLAUDE_CODE_PLUGIN_CACHE_DIR" {
				return cache
			}
			return ""
		},
	})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertObservation(t, result, Claude, ComponentPlugin, "overridden-plugin")
}

func TestSkillDependenciesKeepDedicatedPackagesAndExcludeCarriers(t *testing.T) {
	home := t.TempDir()
	skillFile := filepath.Join(home, ".agents", "skills", "reporter", "SKILL.md")
	mustWriteFile(t, skillFile, `---
name: reporter
---

Run the package explicitly:

    npx -y @scope/report-cli@^2.1
    uvx python-report==1.4.0

`+"```sh"+`
git status
node script.js
reportctl render input.json
`+"```"+`
`)
	result, err := NewCodex().Scan(context.Background(), ScanOptions{
		HomeDir: home, CodexSystemDir: filepath.Join(home, "missing-system"),
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := assertObservation(t, result, Codex, ComponentSkill, "reporter")
	identities := map[string]string{}
	for _, dependency := range observation.Dependencies {
		identities[dependency.PackageIdentity] = dependency.Constraint
		if dependency.EvidencePath != skillFile || dependency.EvidenceLine == 0 {
			t.Fatalf("dependency evidence missing: %#v", dependency)
		}
	}
	for identity, constraint := range map[string]string{
		"npm:@scope/report-cli": "^2.1", "pypi:python-report": "==1.4.0", "cli:reportctl": "",
	} {
		if got, ok := identities[identity]; !ok || got != constraint {
			t.Errorf("dependency %s=%q, all=%#v", identity, got, identities)
		}
	}
	for _, carrier := range []string{"cli:git", "cli:node", "cli:npx", "cli:uvx"} {
		if _, ok := identities[carrier]; ok {
			t.Errorf("carrier was treated as a managed dependency: %s", carrier)
		}
	}
}

func assertObservation(t *testing.T, result Result, host Name, kind ComponentKind, name string) Observation {
	t.Helper()
	for _, observation := range result.Observations {
		if observation.Host == host && observation.Kind == kind && observation.Name == name {
			return observation
		}
	}
	t.Fatalf("missing observation host=%s kind=%s name=%s", host, kind, name)
	return Observation{}
}

func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	mustWriteFile(t, path, string(data))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", path, err)
	}
}
