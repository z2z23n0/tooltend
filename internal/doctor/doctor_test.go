package doctor

import (
	"context"
	"os"
	"path/filepath"
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
