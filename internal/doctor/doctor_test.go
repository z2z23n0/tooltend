package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/store"
)

type fakeRunner struct{ calls int }

func (r *fakeRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	r.calls++
	return execx.Result{}, nil
}

func TestFullRepairPlanIsReadOnlyUntilConfirmedAndRepairsAllIntegrations(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "tooltend-home")
	paths := config.ResolveWith(home, func(name string) string {
		if name == config.EnvHome {
			return root
		}
		return ""
	})
	executable := filepath.Join(home, "bin", "tooltend")
	runner := &fakeRunner{}
	options := Options{Paths: paths, Home: home, Executable: executable, Agents: []model.HostKind{model.HostCodex, model.HostClaude}, Runner: runner}

	repair, err := RepairPlanWithOptions(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ConfigFile, paths.DatabaseFile, filepath.Join(home, ".codex", "hooks.json"), filepath.Join(home, ".claude", "settings.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("planning wrote %s: %v", path, err)
		}
	}
	if _, err := repair.Apply(context.Background(), plan.ApplyOptions{}); !os.IsNotExist(err) && err != plan.ErrConfirmationRequired {
		t.Fatalf("unconfirmed apply error = %v", err)
	}
	if _, err := repair.Apply(context.Background(), plan.ApplyOptions{Confirmed: true}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ConfigFile, paths.DatabaseFile, filepath.Join(home, ".codex", "hooks.json"), filepath.Join(home, ".claude", "settings.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("repair did not write %s: %v", path, err)
		}
	}
	if runner.calls == 0 {
		t.Fatal("repair did not register the scheduler")
	}
	report := RunWithOptions(context.Background(), options)
	for _, check := range report.Checks {
		if check.Name == "config" || check.Name == "database" || check.Name == "codex_hooks" || check.Name == "claude_hooks" || check.Name == "scheduler" {
			if check.Level != LevelOK {
				t.Fatalf("check %#v", check)
			}
		}
	}
	second, err := RepairPlanWithOptions(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range second.Preview().Operations {
		if operation.ID == "repair-scheduler" {
			t.Fatalf("healthy scheduler was rewritten by a second repair: %#v", operation)
		}
	}
}

func TestSchedulerFileContentMatchesFileRole(t *testing.T) {
	executable := "/opt/tooltend/bin/tooltend"
	stateDir := "/var/lib/tooltend-state"
	service := `[Unit]
Description=ToolTend one-shot reconciliation

[Service]
Type=oneshot
Environment="PATH=/opt/tooltend/bin:/usr/bin:/bin"
ExecStart="/opt/tooltend/bin/tooltend" reconcile --once --state-dir "/var/lib/tooltend-state" --json
`
	watchdogService := `[Unit]
Description=Check ToolTend scheduled reconciliation

[Service]
Type=oneshot
Environment="PATH=/opt/tooltend/bin:/usr/bin:/bin"
ExecStart="/opt/tooltend/bin/tooltend" watchdog --max-age 3h --state-dir "/var/lib/tooltend-state" --json
`
	plist := `<plist><dict>
<key>ProgramArguments</key><array>
<string>/opt/tooltend/bin/tooltend</string><string>reconcile</string><string>--once</string>
<string>--state-dir</string><string>/var/lib/tooltend-state</string><string>--json</string>
</array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/opt/tooltend/bin:/usr/bin:/bin</string></dict>
<key>StandardOutPath</key><string>/var/lib/tooltend-state/logs/reconcile.stdout.log</string>
<key>StandardErrorPath</key><string>/var/lib/tooltend-state/logs/reconcile.stderr.log</string>
</dict></plist>`
	watchdogPlist := `<plist><dict>
<key>ProgramArguments</key><array>
<string>/opt/tooltend/bin/tooltend</string><string>watchdog</string><string>--max-age</string><string>2h</string>
<string>--state-dir</string><string>/var/lib/tooltend-state</string><string>--json</string>
</array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/opt/tooltend/bin:/usr/bin:/bin</string></dict>
<key>StandardOutPath</key><string>/var/lib/tooltend-state/logs/watchdog.stdout.log</string>
<key>StandardErrorPath</key><string>/var/lib/tooltend-state/logs/watchdog.stderr.log</string>
</dict></plist>`
	timer := `[Unit]
Description=Run ToolTend reconciliation daily

[Timer]
OnCalendar=*-*-* 03:17:00
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
`
	tests := []struct {
		name     string
		file     string
		content  string
		exe      string
		stateDir string
		want     bool
	}{
		{name: "plist", file: "io.tooltend.reconcile.plist", content: plist, exe: executable, stateDir: stateDir, want: true},
		{name: "watchdog plist", file: "io.tooltend.watchdog.plist", content: watchdogPlist, exe: executable, stateDir: stateDir, want: true},
		{name: "plist missing path", file: "io.tooltend.reconcile.plist", content: strings.Replace(plist, "<key>PATH</key>", "<key>OLD_PATH</key>", 1), exe: executable, stateDir: stateDir},
		{name: "plist wrong argv prefix", file: "io.tooltend.reconcile.plist", content: strings.Replace(plist, "<string>/opt/tooltend/bin/tooltend</string>", "<string>/opt/tooltend/bin/tooltend-bundle-driver</string><string>/opt/tooltend/bin/tooltend</string>", 1), exe: executable, stateDir: stateDir},
		{name: "service", file: "tooltend-reconcile.service", content: service, exe: executable, stateDir: stateDir, want: true},
		{name: "watchdog service", file: "tooltend-watchdog.service", content: watchdogService, exe: executable, stateDir: stateDir, want: true},
		{name: "timer", file: "tooltend-reconcile.timer", content: timer, exe: executable, stateDir: stateDir, want: true},
		{name: "timer missing calendar", file: "tooltend-reconcile.timer", content: strings.Replace(timer, "OnCalendar=", "Calendar=", 1), exe: executable, stateDir: stateDir},
		{name: "timer missing persistence", file: "tooltend-reconcile.timer", content: strings.Replace(timer, "Persistent=true", "Persistent=false", 1), exe: executable, stateDir: stateDir},
		{name: "service missing once", file: "tooltend-reconcile.service", content: strings.Replace(service, "--once", "--continuous", 1), exe: executable, stateDir: stateDir},
		{name: "service missing path", file: "tooltend-reconcile.service", content: strings.Replace(service, "Environment=", "EnvironmentFile=", 1), exe: executable, stateDir: stateDir},
		{name: "service wrong executable", file: "tooltend-reconcile.service", content: service, exe: "/opt/tooltend/bin/other", stateDir: stateDir},
		{name: "service wrong state", file: "tooltend-reconcile.service", content: service, exe: executable, stateDir: "/var/lib/other-state"},
		{name: "unknown file", file: "schedule.txt", content: service, exe: executable, stateDir: stateDir},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := schedulerFileContentMatches(test.file, test.content, test.exe, test.stateDir); got != test.want {
				t.Fatalf("schedulerFileContentMatches()=%v want=%v", got, test.want)
			}
		})
	}
}

func TestLaunchdLastExitCode(t *testing.T) {
	code, ok := launchdLastExitCode("state = not running\n\tlast exit code = 17\n")
	if !ok || code != 17 {
		t.Fatalf("code=%d ok=%v", code, ok)
	}
}

func TestReconcileRunCheckReportsFailureAndStaleness(t *testing.T) {
	database, err := store.OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)
	if check := checkReconcileRun(context.Background(), database, 24*time.Hour, now); check.Level != LevelWarning {
		t.Fatalf("missing check = %#v", check)
	}
	if _, err := database.DB().Exec(`INSERT INTO reconcile_runs(id,reason,status,started_at,finished_at,error_code,summary_json) VALUES('run','scheduled','failed',?,?, 'task_failed','{}')`, now.Add(-time.Hour).Format(time.RFC3339Nano), now.Add(-59*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if check := checkReconcileRun(context.Background(), database, 24*time.Hour, now); check.Level != LevelError || !strings.Contains(check.Message, "task_failed") {
		t.Fatalf("failed check = %#v", check)
	}
}

func TestRepairPlanRefusesSchedulerFileChangedAfterPreview(t *testing.T) {
	home := t.TempDir()
	paths := config.ResolveWith(home, func(name string) string {
		if name == config.EnvHome {
			return filepath.Join(home, "tooltend-home")
		}
		return ""
	})
	options := Options{Paths: paths, Home: home, Executable: filepath.Join(home, "bin", "tooltend"), Runner: &fakeRunner{}}
	repair, err := RepairPlanWithOptions(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	var schedulerPath string
	for _, operation := range repair.Preview().Operations {
		if operation.ID == "repair-scheduler" {
			schedulerPath = operation.Target
		}
	}
	if schedulerPath == "" {
		t.Skip("platform does not support scheduler repair")
	}
	if err := os.MkdirAll(filepath.Dir(schedulerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	external := []byte("externally changed schedule\n")
	if err := os.WriteFile(schedulerPath, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repair.Apply(context.Background(), plan.ApplyOptions{Confirmed: true}); err == nil {
		t.Fatal("scheduler race was not rejected")
	}
	got, err := os.ReadFile(schedulerPath)
	if err != nil || string(got) != string(external) {
		t.Fatalf("external scheduler change was overwritten: %q err=%v", got, err)
	}
}

func TestRepairPlanRefusesExistingInvalidConfiguration(t *testing.T) {
	home := t.TempDir()
	paths := config.ResolveWith(home, func(name string) string {
		if name == config.EnvHome {
			return filepath.Join(home, "tooltend-home")
		}
		return ""
	})
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	invalid := []byte("version = 1\ninvalid = [\n")
	if err := os.WriteFile(paths.ConfigFile, invalid, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RepairPlanWithOptions(context.Background(), Options{Paths: paths, Home: home, Executable: "/bin/tooltend"})
	if err == nil {
		t.Fatal("invalid existing configuration was accepted for replacement")
	}
	got, readErr := os.ReadFile(paths.ConfigFile)
	if readErr != nil || string(got) != string(invalid) {
		t.Fatalf("invalid configuration changed during planning: %q err=%v", got, readErr)
	}
}
