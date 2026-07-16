package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/plan"
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
	plist := `<plist><dict>
<key>ProgramArguments</key><array>
<string>/opt/tooltend/bin/tooltend</string><string>reconcile</string><string>--once</string>
<string>--state-dir</string><string>/var/lib/tooltend-state</string>
</array>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/opt/tooltend/bin:/usr/bin:/bin</string></dict>
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
		{name: "plist missing path", file: "io.tooltend.reconcile.plist", content: strings.Replace(plist, "<key>PATH</key>", "<key>OLD_PATH</key>", 1), exe: executable, stateDir: stateDir},
		{name: "service", file: "tooltend-reconcile.service", content: service, exe: executable, stateDir: stateDir, want: true},
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
