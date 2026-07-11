package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestParseInstallerIntent(t *testing.T) {
	tests := []struct {
		command   string
		installer string
		name      string
		version   string
		ok        bool
	}{
		{"npm install -g @act0r/sherlog@0.3.17", "npm", "@act0r/sherlog", "0.3.17", true},
		{"npx --yes prettier@3.6.2", "npx", "prettier", "3.6.2", true},
		{"pipx install sherlog==0.3.17", "pipx", "sherlog", "0.3.17", true},
		{"uv tool install Ruff@0.12.0", "uv", "ruff", "0.12.0", true},
		{"brew install owner/tap/tool", "homebrew", "owner/tap/tool", "", true},
		{"npm install foo && curl bad", "", "", "", false},
		{"npx --api-key looks-like-a-package-secret actual-package", "", "", "", false},
		{"npm install --registry private-registry-token actual-package", "", "", "", false},
		{"bash -lc npm-install", "", "", "", false},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			inputBytes, _ := json.Marshal(ToolInput{Command: test.command})
			got, ok := ParseInstallerIntent(Input{ToolName: "Bash", ToolInput: inputBytes})
			if ok != test.ok || got.Installer != test.installer || got.PackageIdentity != test.name || got.RequestedVersion != test.version {
				t.Fatalf("got %#v, %v", got, ok)
			}
		})
	}
}

type eventRecorder struct{ events []Event }

func (r *eventRecorder) TryRecord(_ context.Context, event Event) error {
	r.events = append(r.events, event)
	return nil
}

func TestHandlerStoresOnlyNormalizedFields(t *testing.T) {
	recorder := &eventRecorder{}
	payload := `{"session_id":"secret-session","cwd":"/private/project","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"tool-1","tool_input":{"command":"npm install -g foo@1.2.3"},"transcript_path":"/secret/transcript"}`
	handler := Handler{Host: "codex", Events: recorder, Now: func() time.Time { return time.Unix(1, 0) }}
	if err := handler.Run(context.Background(), bytes.NewBufferString(payload), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("got %d events", len(recorder.events))
	}
	event := recorder.events[0]
	if event.PackageIdentity != "foo" || event.RequestedVersion != "1.2.3" || event.ProjectHash == "/private/project" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestHandlerFailsOpenOnOversizedInput(t *testing.T) {
	handler := Handler{}
	if err := handler.Run(context.Background(), bytes.NewReader(make([]byte, MaxInputBytes+1)), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

type busySink struct{}

func (busySink) TryRecord(context.Context, Event) error { return errors.New("database is busy") }

func TestHandlerBusySinkFailsOpenWithinHotPathBudget(t *testing.T) {
	payload := []byte(`{"session_id":"s","cwd":"/project","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"t","tool_input":{"command":"npm install foo@1.0.0"}}`)
	handler := Handler{Host: "codex", Events: busySink{}}
	durations := make([]time.Duration, 50)
	for i := range durations {
		started := time.Now()
		if err := handler.Run(context.Background(), bytes.NewReader(payload), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		durations[i] = time.Since(started)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[47]
	if p95 >= 50*time.Millisecond {
		t.Fatalf("hook p95 = %s, want < 50ms", p95)
	}
}
