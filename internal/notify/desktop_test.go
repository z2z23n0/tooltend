package notify

import (
	"context"
	"errors"
	"path/filepath"
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

func TestDarwinNotificationUsesStableAppIdentity(t *testing.T) {
	runner := &recordingRunner{}
	appPath := filepath.Join(t.TempDir(), "ToolTend Notifier")
	err := (Desktop{GOOS: "darwin", AppPath: appPath, Runner: runner}).Send(context.Background(), `Tool"Tend`, "line 1\nline 2")
	if err != nil {
		t.Fatal(err)
	}
	if runner.name != appPath || len(runner.args) != 2 {
		t.Fatalf("call = %s %#v", runner.name, runner.args)
	}
	if runner.args[0] != `Tool"Tend` || runner.args[1] != "line 1\nline 2" {
		t.Fatalf("notification arguments = %#v", runner.args)
	}
}

func TestDarwinNotificationRequiresInstalledNotifier(t *testing.T) {
	err := (Desktop{GOOS: "darwin", Runner: &recordingRunner{}}).Send(context.Background(), "ToolTend", "message")
	if !errors.Is(err, ErrNotifierUnavailable) {
		t.Fatalf("error = %v", err)
	}
}
