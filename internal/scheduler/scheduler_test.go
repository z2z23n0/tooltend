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

func TestRenderedSchedulesIncludeWorkerPATH(t *testing.T) {
	options := Options{
		Executable: "/Users/example/.local/bin/tooltend",
		StateDir:   "/tmp/state",
		PathEnv:    "relative:/opt/homebrew/bin:/usr/bin:/opt/homebrew/bin",
		Hour:       1,
		Minute:     2,
	}
	for name, content := range map[string]string{
		"launchd": renderLaunchd(options),
		"systemd": renderSystemdService(options),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(content, "PATH") || !strings.Contains(content, "/Users/example/.local/bin:/opt/homebrew/bin:/usr/bin") {
				t.Fatalf("schedule does not preserve the ToolTend executable path: %s", content)
			}
			if strings.Contains(content, "relative") || strings.Count(content, "/opt/homebrew/bin") != 1 {
				t.Fatalf("schedule contains an unsafe or duplicate PATH entry: %s", content)
			}
		})
	}
}

func TestRenderedLaunchdSchedulesPersistLogsAndRunWatchdogLater(t *testing.T) {
	options := Options{Executable: "/Users/example/.local/bin/tooltend", StateDir: "/Users/example/.local/state/tooltend", Hour: 23, Minute: 40}
	reconcile := renderLaunchd(options)
	watchdog := renderLaunchdWatchdog(options)
	for _, value := range []string{"reconcile.stdout.log", "reconcile.stderr.log"} {
		if !strings.Contains(reconcile, value) {
			t.Fatalf("reconcile schedule missing %s: %s", value, reconcile)
		}
	}
	if !strings.Contains(watchdog, "<string>watchdog</string>") || !strings.Contains(watchdog, "watchdog.stderr.log") ||
		!strings.Contains(watchdog, "<key>Hour</key><integer>0</integer>") || !strings.Contains(watchdog, "<key>Minute</key><integer>40</integer>") {
		t.Fatalf("invalid watchdog schedule: %s", watchdog)
	}
}

func TestSystemdWatchdogWaitsPastReconcileJitter(t *testing.T) {
	options := Options{Executable: "/usr/local/bin/tooltend", StateDir: "/var/lib/tooltend", Hour: 23, Minute: 40}
	service := renderSystemdWatchdogService(options)
	timer := renderSystemdWatchdogTimer(options)
	if !strings.Contains(service, "watchdog --max-age 3h") || !strings.Contains(timer, "OnCalendar=*-*-* 01:40:00") {
		t.Fatalf("service=%s\ntimer=%s", service, timer)
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
	launchd := Plan{Platform: "launchd", Files: []File{{Path: "/tmp/io.tooltend.reconcile.plist"}, {Path: "/tmp/io.tooltend.watchdog.plist"}}}
	if err := Activate(context.Background(), launchd, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 || !strings.Contains(runner.calls[0], "bootout") || !strings.Contains(runner.calls[1], "bootstrap") || !strings.Contains(runner.calls[3], "watchdog") {
		t.Fatalf("launchd calls = %#v", runner.calls)
	}

	runner.calls = nil
	systemd := Plan{Platform: "systemd", Files: []File{{Path: "/tmp/tooltend-reconcile.service"}, {Path: "/tmp/tooltend-reconcile.timer"}, {Path: "/tmp/tooltend-watchdog.service"}, {Path: "/tmp/tooltend-watchdog.timer"}}}
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
	if err := Activate(context.Background(), Plan{Platform: "launchd", Files: []File{{Path: "/tmp/io.tooltend.reconcile.plist"}}}, &recordingRunner{}); err == nil {
		t.Fatal("expected launchd plan without watchdog to be rejected")
	}
}
