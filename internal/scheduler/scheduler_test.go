package scheduler

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/z2z23n0/tooltend/internal/execx"
)

func TestBuildPlan(t *testing.T) {
	plan, err := BuildPlan(Options{Executable: "/tmp/tooltend", Home: t.TempDir(), StateDir: "/tmp/state", Hour: 4, Minute: 12})
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("expected unsupported platform")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) == 0 || !strings.Contains(string(plan.Files[0].Content), "tooltend") {
		t.Fatalf("invalid plan: %#v", plan)
	}
}

func TestXMLAndSystemdEscaping(t *testing.T) {
	launchd := renderLaunchd(Options{Executable: `/tmp/a&b`, StateDir: `/tmp/<state>`, Hour: 1, Minute: 2})
	if strings.Contains(launchd, "/tmp/a&b") || !strings.Contains(launchd, "a&amp;b") {
		t.Fatalf("not escaped: %s", launchd)
	}
	quoted := systemdQuote(`/tmp/a"b`)
	if !strings.Contains(quoted, `\"`) {
		t.Fatalf("not quoted: %s", quoted)
	}
}

type recordingRunner struct {
	calls []string
	fail  string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if r.fail != "" && strings.Contains(call, r.fail) {
		return execx.Result{}, errors.New("failed")
	}
	return execx.Result{}, nil
}

func TestActivateRegistersOneShotSchedule(t *testing.T) {
	runner := &recordingRunner{}
	launchd := Plan{Platform: "launchd", Files: []File{{Path: "/tmp/io.tooltend.reconcile.plist"}}}
	if err := Activate(context.Background(), launchd, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], "bootout") || !strings.Contains(runner.calls[1], "bootstrap") {
		t.Fatalf("launchd calls = %#v", runner.calls)
	}

	runner.calls = nil
	systemd := Plan{Platform: "systemd", Files: []File{{Path: "/tmp/tooltend-reconcile.service"}, {Path: "/tmp/tooltend-reconcile.timer"}}}
	if err := Activate(context.Background(), systemd, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || !strings.Contains(runner.calls[0], "daemon-reload") || !strings.Contains(runner.calls[1], "enable --now") {
		t.Fatalf("systemd calls = %#v", runner.calls)
	}
}

func TestActivateFailsClosedForMalformedPlan(t *testing.T) {
	if err := Activate(context.Background(), Plan{Platform: "launchd"}, &recordingRunner{}); err == nil {
		t.Fatal("expected malformed launchd plan to be rejected")
	}
}
