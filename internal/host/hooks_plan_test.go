package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexHookPlanPreservesUnknownFieldsAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".codex", "hooks.json")
	mustWriteFile(t, target, `{
  "unknownTopLevel": {"large": 9007199254740993},
  "hooks": {
    "PreToolUse": [{
      "matcher": "Bash|apply_patch|mcp__.*",
      "unknownGroupField": "keep-me",
      "hooks": [{"type":"command","command":"/usr/bin/existing","unknownHandlerField":true}]
    }]
  }
}`)
	binary := filepath.Join(home, "bin", "tooltend shim")

	plan, err := NewCodex().PlanHookInstall(context.Background(), HookInstallOptions{
		HomeDir: home, BinaryPath: binary, Scope: ScopeUser,
	})
	if err != nil {
		t.Fatalf("PlanHookInstall() error = %v", err)
	}
	mutation := onlyMutation(t, plan)
	if !mutation.Changed || len(mutation.Operations) != 3 {
		t.Fatalf("first mutation changed=%t operations=%d, want true/3", mutation.Changed, len(mutation.Operations))
	}
	document := decodeDocument(t, mutation.Content)
	unknown := document["unknownTopLevel"].(map[string]any)
	if got := unknown["large"].(json.Number).String(); got != "9007199254740993" {
		t.Fatalf("unknown large integer = %s", got)
	}
	preGroup := hookGroups(t, document, "PreToolUse")[0].(map[string]any)
	if preGroup["unknownGroupField"] != "keep-me" {
		t.Fatal("unknown group field was not preserved")
	}
	assertToolTendHookModes(t, document, Codex, binary)

	if err := os.WriteFile(target, mutation.Content, mutation.Mode); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	second, err := NewCodex().PlanHookInstall(context.Background(), HookInstallOptions{
		HomeDir: home, BinaryPath: binary, Scope: ScopeUser,
	})
	if err != nil {
		t.Fatalf("second PlanHookInstall() error = %v", err)
	}
	secondMutation := onlyMutation(t, second)
	if secondMutation.Changed || len(secondMutation.Operations) != 0 {
		t.Fatalf("idempotent mutation changed=%t operations=%d", secondMutation.Changed, len(secondMutation.Operations))
	}
}

func TestClaudeHookPlanUsesAsyncOnlyForToolEvents(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".claude", "settings.json")
	mustWriteFile(t, target, `{
  "permissions": {"allow": ["Bash(git status)"]},
  "hooks": {
    "SessionStart": [{
      "matcher": "startup|resume|clear|compact",
      "hooks": [{"type":"command","command":"/usr/bin/existing"}]
    }]
  }
}`)
	binary := filepath.Join(home, "bin", "tooltend")

	plan, err := NewClaude().PlanHookInstall(context.Background(), HookInstallOptions{
		HomeDir: home, BinaryPath: binary,
	})
	if err != nil {
		t.Fatalf("PlanHookInstall() error = %v", err)
	}
	mutation := onlyMutation(t, plan)
	document := decodeDocument(t, mutation.Content)
	if _, ok := document["permissions"]; !ok {
		t.Fatal("Claude settings fields unrelated to hooks were not preserved")
	}
	assertToolTendHookModes(t, document, Claude, binary)
}

func TestHookPlanRejectsInvalidHookShapeWithoutWriting(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".codex", "hooks.json")
	original := `{"hooks":"leave-this-alone"}`
	mustWriteFile(t, target, original)

	_, err := NewCodex().PlanHookInstall(context.Background(), HookInstallOptions{
		HomeDir: home, BinaryPath: "/usr/local/bin/tooltend",
	})
	if err == nil || !strings.Contains(err.Error(), "hooks field must be an object") {
		t.Fatalf("PlanHookInstall() error = %v", err)
	}
	content, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("os.ReadFile() error = %v", readErr)
	}
	if string(content) != original {
		t.Fatal("planning modified the hook target")
	}
}

func TestHookPlanTargetsProjectLayers(t *testing.T) {
	project := t.TempDir()
	binary := "/usr/local/bin/tooltend"

	codexPlan, err := NewCodex().PlanHookInstall(context.Background(), HookInstallOptions{
		HomeDir: t.TempDir(), Project: project, BinaryPath: binary, Scope: ScopeProject,
	})
	if err != nil {
		t.Fatalf("Codex PlanHookInstall() error = %v", err)
	}
	if got, want := onlyMutation(t, codexPlan).Path, filepath.Join(project, ".codex", "hooks.json"); got != want {
		t.Fatalf("Codex target = %q, want %q", got, want)
	}

	claudePlan, err := NewClaude().PlanHookInstall(context.Background(), HookInstallOptions{
		HomeDir: t.TempDir(), Project: project, BinaryPath: binary, Scope: ScopeLocal,
	})
	if err != nil {
		t.Fatalf("Claude PlanHookInstall() error = %v", err)
	}
	if got, want := onlyMutation(t, claudePlan).Path, filepath.Join(project, ".claude", "settings.local.json"); got != want {
		t.Fatalf("Claude target = %q, want %q", got, want)
	}
	if len(claudePlan.Warnings) != 1 || claudePlan.Warnings[0].Code != "local_hook_config_uncommitted" {
		t.Fatalf("Claude local warnings = %#v", claudePlan.Warnings)
	}
}

func TestManagedHookIdentitySurvivesBinaryRelocation(t *testing.T) {
	oldCommand := hookCommand("/old location/tooltend", Codex, "SessionStart")
	desired := hookCommand("/new/location/tooltend", Codex, "SessionStart")
	if !isToolTendHook(map[string]any{"command": oldCommand}, desired) {
		t.Fatal("expected relocated ToolTend hook to remain managed")
	}
	if isToolTendHook(map[string]any{"command": hookCommand("/old/location/not-tooltend", Codex, "SessionStart")}, desired) {
		t.Fatal("unrelated executable must not be adopted as a ToolTend hook")
	}
	quoted := hookCommand("/tmp/it's/tooltend", Claude, "PostToolUse")
	binary, hostName, event, ok := parseManagedHookCommand(quoted)
	if !ok || binary != "/tmp/it's/tooltend" || hostName != string(Claude) || event != "PostToolUse" {
		t.Fatalf("parsed %q %q %q %t", binary, hostName, event, ok)
	}
}

func TestApplyMutationRejectsConcurrentHookEdit(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, ".codex", "hooks.json")
	mustWriteFile(t, target, `{"hooks":{}}`)
	planned, err := NewCodex().PlanHookInstall(context.Background(), HookInstallOptions{HomeDir: home, BinaryPath: "/usr/local/bin/tooltend"})
	if err != nil {
		t.Fatal(err)
	}
	mutation := onlyMutation(t, planned)
	concurrent := []byte(`{"hooks":{},"user_edit":true}`)
	if err := os.WriteFile(target, concurrent, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMutation(mutation); err == nil {
		t.Fatal("expected concurrent edit to reject stale mutation")
	}
	if got, _ := os.ReadFile(target); string(got) != string(concurrent) {
		t.Fatalf("concurrent edit was overwritten: %s", got)
	}
}

func onlyMutation(t *testing.T, plan MutationPlan) FileMutation {
	t.Helper()
	if len(plan.Mutations) != 1 {
		t.Fatalf("mutations = %d, want 1", len(plan.Mutations))
	}
	return plan.Mutations[0]
}

func decodeDocument(t *testing.T, content []byte) map[string]any {
	t.Helper()
	var document map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode planned JSON: %v", err)
	}
	return document
}

func hookGroups(t *testing.T, document map[string]any, event string) []any {
	t.Helper()
	hooks, ok := document["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks is %T", document["hooks"])
	}
	groups, ok := hooks[event].([]any)
	if !ok {
		t.Fatalf("hooks[%q] is %T", event, hooks[event])
	}
	return groups
}

func assertToolTendHookModes(t *testing.T, document map[string]any, host Name, binary string) {
	t.Helper()
	for _, event := range []string{"SessionStart", "PreToolUse", "PostToolUse"} {
		command := hookCommand(binary, host, event)
		found := false
		for _, groupValue := range hookGroups(t, document, event) {
			group, ok := groupValue.(map[string]any)
			if !ok {
				continue
			}
			handlers, _ := group["hooks"].([]any)
			for _, handlerValue := range handlers {
				handler, ok := handlerValue.(map[string]any)
				if !ok || handler["command"] != command {
					continue
				}
				found = true
				async, hasAsync := handler["async"].(bool)
				if host == Claude && event != "SessionStart" {
					if !hasAsync || !async {
						t.Errorf("%s %s hook async = %#v, want true", host, event, handler["async"])
					}
				} else if hasAsync {
					t.Errorf("%s %s hook unexpectedly has async=%t", host, event, async)
				}
			}
		}
		if !found {
			t.Errorf("missing ToolTend %s %s hook", host, event)
		}
	}
}
