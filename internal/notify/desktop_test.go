package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/z2z23n0/tooltend/internal/execx"
)

type recordingRunner struct {
	name string
	args []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.name, r.args = name, append([]string(nil), args...)
	return execx.Result{}, nil
}

func TestDarwinNotificationEscapesAppleScript(t *testing.T) {
	runner := &recordingRunner{}
	err := (Desktop{GOOS: "darwin", Runner: runner}).Send(context.Background(), `Tool"Tend`, "line 1\nline 2")
	if err != nil {
		t.Fatal(err)
	}
	if runner.name != "/usr/bin/osascript" || len(runner.args) != 2 || runner.args[0] != "-e" {
		t.Fatalf("call = %s %#v", runner.name, runner.args)
	}
	if strings.Contains(runner.args[1], "\n") || !strings.Contains(runner.args[1], `Tool\"Tend`) {
		t.Fatalf("unsafe script = %q", runner.args[1])
	}
}
