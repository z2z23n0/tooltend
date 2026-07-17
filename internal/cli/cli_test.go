package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/z2z23n0/tooltend/internal/api/v1"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

func TestCommandTreeContainsCompleteV1Surface(t *testing.T) {
	command := New(testOptions(t, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader("")))
	for _, path := range []string{
		"init", "scan", "status", "components list", "components show", "policy set",
		"bundles list", "bundles show", "bundles configure", "bundles update", "bundles rollback", "bundles history", "bundles doctor",
		"update", "review", "history", "rollback", "adopt", "project init", "project export",
		"project sync", "self status", "self update", "doctor", "hook", "kick", "reconcile", "version",
	} {
		if _, _, err := command.Find(strings.Fields(path)); err != nil {
			t.Fatalf("missing command %q: %v", path, err)
		}
	}
	for _, flag := range []string{"json", "dry-run", "yes", "config", "state-dir", "no-color"} {
		if command.PersistentFlags().Lookup(flag) == nil {
			t.Fatalf("missing global flag --%s", flag)
		}
	}
}

func TestInitDiscoversUnconfiguredBundlesWithoutSchedulingLegacyTasks(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	options.Runner = &successfulRunner{}
	command := New(options)
	command.SetArgs([]string{"init", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("init: %v\nstdout=%s\nstderr=%s", err, out.String(), stderr.String())
	}
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	database, err := store.OpenReadOnly(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	counts, err := database.BundleCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total == 0 || counts.Configured != 0 || counts.Managed != 0 || counts.Unconfigured != counts.Total {
		t.Fatalf("bundle counts = %+v", counts)
	}
	legacy, err := database.CountTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Pending+legacy.Running != 0 {
		t.Fatalf("legacy tasks were scheduled: %+v", legacy)
	}
	var bundleTasks int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM bundle_tasks`).Scan(&bundleTasks); err != nil || bundleTasks != 0 {
		t.Fatalf("bundle tasks=%d err=%v", bundleTasks, err)
	}
}

func TestResetStateDryRunIsReadOnlyAndConfirmedResetBacksUp(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	options.Runner = &successfulRunner{}
	command := New(options)
	command.SetArgs([]string{"init", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	marker := filepath.Join(paths.ObjectsDir, "keep-until-confirmed")
	if err := os.WriteFile(marker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupParent := filepath.Join(filepath.Dir(paths.StateDir), "tooltend-backups")
	out.Reset()
	command = New(options)
	command.SetArgs([]string{"init", "--reset-state", "--dry-run", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("reset dry-run: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dry-run changed old state: %v", err)
	}
	if _, err := os.Stat(backupParent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created backup directory: %v", err)
	}
	out.Reset()
	command = New(options)
	command.SetArgs([]string{"init", "--reset-state", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("reset: %v\nstdout=%s\nstderr=%s", err, out.String(), stderr.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed reset retained old marker: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(backupParent, "*", "manifest.json"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
	database, err := store.OpenReadOnly(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	counts, err := database.BundleCounts(context.Background())
	if err != nil || counts.Total == 0 || counts.Configured != 0 || counts.Managed != 0 {
		t.Fatalf("post-reset counts=%+v err=%v", counts, err)
	}
}

func TestResetStateRestoresOldStateWhenSchedulerReactivationFails(t *testing.T) {
	var out, stderr bytes.Buffer
	runner := &successfulRunner{}
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	options.Runner = runner
	command := New(options)
	command.SetArgs([]string{"init", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	marker := filepath.Join(paths.ObjectsDir, "restore-me")
	if err := os.WriteFile(marker, []byte("old-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	configHash := fileHashOrEmpty(paths.ConfigFile)
	databaseHash := fileHashOrEmpty(paths.DatabaseFile)
	runner.failAt = runner.calls + 3 // deactivate and best-effort bootout succeed; final registration fails once
	out.Reset()
	command = New(options)
	command.SetArgs([]string{"init", "--reset-state", "--yes", "--json"})
	if err := command.Execute(); err == nil || !IsReported(err) {
		t.Fatalf("reset unexpectedly succeeded: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "old-state" {
		t.Fatalf("old data was not restored: data=%q err=%v", data, err)
	}
	if fileHashOrEmpty(paths.ConfigFile) != configHash || fileHashOrEmpty(paths.DatabaseFile) != databaseHash {
		t.Fatal("old configuration or database was not restored byte-for-byte")
	}
}

func TestResetStateRefusesConfiguredManagedBundle(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	options.Runner = &successfulRunner{}
	command := New(options)
	command.SetArgs([]string{"init", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := database.ListBundles(context.Background())
	if err != nil || len(bundles) == 0 {
		t.Fatalf("bundles=%v err=%v", bundles, err)
	}
	if err := database.ConfigureBundle(context.Background(), model.BundlePolicy{BundleID: bundles[0].ID, Mode: model.BundlePolicyManual, RecipeTrusted: true, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	command = New(options)
	command.SetArgs([]string{"init", "--reset-state", "--dry-run", "--json"})
	err = command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("configured bundle reset unexpectedly succeeded: %v", err)
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == nil || envelope.Error.Code != "reset_refused" {
		t.Fatalf("unexpected reset error: %+v", envelope.Error)
	}
}

func TestBundleConfigureLeavesSkippedBundlesUnconfiguredAndRejectsUnsafeAuto(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader("\n"))
	options.Runner = &successfulRunner{}
	command := New(options)
	command.SetArgs([]string{"init", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	command = New(options)
	command.SetArgs([]string{"bundles", "configure", "--yes"})
	if err := command.Execute(); err != nil {
		t.Fatalf("skip configure: %v", err)
	}
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	database, err := store.OpenReadOnly(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := database.BundleCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Configured != 0 || counts.Managed != 0 {
		t.Fatalf("skipped bundles were configured: %+v", counts)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	command = New(options)
	command.SetArgs([]string{"bundles", "configure", "--set", "tooltend=auto", "--yes", "--json"})
	err = command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("unsafe auto configuration unexpectedly succeeded: %v", err)
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error == nil || envelope.Error.Code != "unsafe_policy" {
		t.Fatalf("unexpected configure error: %+v", envelope.Error)
	}
}

func TestComponentsListSeparatesActionableManagedAndCompleteInventory(t *testing.T) {
	var seedOut, seedErr bytes.Buffer
	options := testOptions(t, &seedOut, &seedErr, strings.NewReader(""))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	components := []model.LogicalComponent{
		{ID: "actionable", Kind: model.ComponentSkill, Name: "actionable", LogicalKey: "actionable", CreatedAt: now, UpdatedAt: now},
		{ID: "managed", Kind: model.ComponentSkill, Name: "managed", LogicalKey: "managed", CreatedAt: now, UpdatedAt: now},
		{ID: "host-owned", Kind: model.ComponentSkill, Name: "host-owned", LogicalKey: "host-owned", CreatedAt: now, UpdatedAt: now},
		{ID: "dependency-only", Kind: model.ComponentCLI, Name: "dependency-only", LogicalKey: "dependency-only", CreatedAt: now, UpdatedAt: now},
	}
	for _, component := range components {
		if err := database.UpsertComponent(context.Background(), component); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	bindings := []model.Binding{
		{ID: "binding-actionable", ComponentID: "actionable", Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/skills/actionable", InstallMethod: "observed", Classification: model.ClassificationDetached, LastSeenAt: now},
		{ID: "binding-managed", ComponentID: "managed", Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/skills/managed", InstallMethod: "generation_symlink", Managed: true, Classification: model.ClassificationClean, LastSeenAt: now},
		{ID: "binding-host-owned", ComponentID: "host-owned", Host: model.HostCodex, Scope: model.ScopeGlobal, InstallPath: "/cache/plugin/skill", InstallMethod: "host-owned:codex", Classification: model.ClassificationClean, LastSeenAt: now},
	}
	for _, binding := range bindings {
		if err := database.UpsertBinding(context.Background(), binding); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) []componentSummary {
		t.Helper()
		var out, stderr bytes.Buffer
		runOptions := options
		runOptions.In, runOptions.Out, runOptions.ErrOut = strings.NewReader(""), &out, &stderr
		command := New(runOptions)
		command.SetArgs(append([]string{"components", "list", "--json"}, args...))
		if err := command.Execute(); err != nil {
			t.Fatalf("components list %v: %v\nstderr=%s", args, err, stderr.String())
		}
		var envelope struct {
			Data []componentSummary `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	if got := run(); len(got) != 2 || got[0].Component.Name != "actionable" || got[1].Component.Name != "managed" {
		t.Fatalf("default components = %#v", got)
	}
	if got := run("--managed"); len(got) != 1 || got[0].Component.Name != "managed" {
		t.Fatalf("managed components = %#v", got)
	}
	if got := run("--all"); len(got) != 4 {
		t.Fatalf("all components = %#v", got)
	}
}

type successfulRunner struct {
	calls  int
	failAt int
}

func (r *successfulRunner) Run(context.Context, string, ...string) (execx.Result, error) {
	r.calls++
	if r.failAt > 0 && r.calls == r.failAt {
		return execx.Result{}, errors.New("injected runner failure")
	}
	return execx.Result{}, nil
}

func TestConfirmedInitAppliesCompletePlannedState(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	runner := &successfulRunner{}
	options.Runner = runner
	command := New(options)
	command.SetArgs([]string{"init", "--yes", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("init: %v\nstdout=%s\nstderr=%s", err, out.String(), stderr.String())
	}
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	schedulePath := filepath.Join(options.HomeDir, "Library", "LaunchAgents", "io.tooltend.reconcile.plist")
	if runtime.GOOS == "linux" {
		schedulePath = filepath.Join(options.HomeDir, ".config", "systemd", "user", "tooltend-reconcile.timer")
	}
	for _, path := range []string{
		paths.ConfigFile, paths.DatabaseFile, paths.ShimDir,
		filepath.Join(options.HomeDir, ".profile"),
		filepath.Join(options.HomeDir, ".codex", "hooks.json"),
		filepath.Join(options.HomeDir, ".claude", "settings.json"),
		schedulePath,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("confirmed init did not create %s: %v", path, err)
		}
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0] != options.WorkingDir || cfg.Runtime.ShimDir != paths.ShimDir {
		t.Fatalf("unexpected initialized config: %+v", cfg)
	}
	if runner.calls == 0 {
		t.Fatal("confirmed init wrote schedule files but did not activate the scheduler")
	}
}

func TestInitRuntimeMigrationPreviewCountsOnlyExactSafeCandidates(t *testing.T) {
	report := inventory.Report{HostResult: host.Result{
		Observations: []host.Observation{
			{Key: "mcp", Host: host.Codex, Kind: host.ComponentStdioMCP, Source: host.SourceRef{Kind: "npm", Package: "server", Version: "1.2.3"}, Evidence: []host.Evidence{{Pointer: "mcp_servers.server"}}},
			{Key: "cli", Host: host.Codex, Kind: host.ComponentKind("cli"), Path: "/usr/local/bin/tool", Source: host.SourceRef{Kind: "pypi", Package: "tool", Version: "v2.0.0"}},
			{Key: "floating", Host: host.Codex, Kind: host.ComponentKind("cli"), Path: "/usr/local/bin/floating", Source: host.SourceRef{Kind: "npm", Package: "floating", Version: "latest"}},
			{Key: "skill", Host: host.Codex, Kind: host.ComponentSkill, Dependencies: []host.DependencyRef{{
				PackageIdentity: "npm:report-cli", Source: host.SourceRef{Kind: "npm", Package: "report-cli", Version: "3.0.0"},
				InstallPath: "/usr/local/bin/report-cli", EvidencePath: "/skills/reporter/SKILL.md",
			}}},
		},
		Bindings: []host.Binding{
			{Host: host.Codex, ComponentKey: "mcp", ConfigPath: "/tmp/config.toml"},
			{Host: host.Codex, ComponentKey: "cli", InstallPath: "/usr/local/bin/tool"},
			{Host: host.Codex, ComponentKey: "floating", InstallPath: "/usr/local/bin/floating"},
		},
	}}
	if got := countInitRuntimeMigrationCandidates(report); got != 3 {
		t.Fatalf("runtime migration candidates = %d, want 3", got)
	}
}

func TestJSONWriteRequiresYesAndDoesNotMutate(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	command := New(options)
	command.SetArgs([]string{"project", "init", "--json"})
	err := command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("expected reported confirmation error, got %v", err)
	}
	var envelope v1.Envelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode envelope: %v\n%s", decodeErr, out.String())
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "confirmation_required" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
	for _, name := range []string{"tooltend.toml", "tooltend.lock"} {
		if _, statErr := os.Stat(filepath.Join(options.WorkingDir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s changed before confirmation: %v", name, statErr)
		}
	}
}

func TestDryRunReturnsPreviewWithoutWriting(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	command := New(options)
	command.SetArgs([]string{"project", "init", "--json", "--dry-run"})
	if err := command.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr=%s", err, stderr.String())
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK {
		t.Fatalf("dry run failed: %+v", envelope)
	}
	encoded, _ := json.Marshal(envelope.Data)
	if !bytes.Contains(encoded, []byte(`"dry_run":true`)) || !bytes.Contains(encoded, []byte(`"requires_confirmation":true`)) {
		t.Fatalf("dry-run result lacks preview: %s", encoded)
	}
	if _, err := os.Stat(filepath.Join(options.WorkingDir, "tooltend.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run wrote project manifest: %v", err)
	}
}

func TestAdoptGitSubdirDryRunJSONBindsCanonicalParameter(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAtomic(paths.ConfigFile, config.Default()); err != nil {
		t.Fatal(err)
	}
	installPath := filepath.Join(options.HomeDir, ".codex", "plugins", "demo")
	if err := os.MkdirAll(installPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installPath, "plugin.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.UpsertComponent(context.Background(), model.LogicalComponent{
		ID: "component", Kind: model.ComponentPlugin, Name: "component", LogicalKey: "test:component", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.UpsertBinding(context.Background(), model.Binding{
		ID: "binding", ComponentID: "component", Host: model.HostCodex, Scope: model.ScopeGlobal,
		InstallPath: installPath, InstallMethod: "native", Classification: model.ClassificationUnknown, LastSeenAt: now,
	}); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	command := New(options)
	command.SetArgs([]string{
		"adopt", "component", "--source", "https://example.test/monorepo.git?ignored=query#ignored-fragment",
		"--subdir", "./plugins/demo", "--dry-run", "--json",
	})
	if err := command.Execute(); err != nil {
		t.Fatalf("adopt dry-run: %v\nstdout=%s\nstderr=%s", err, out.String(), stderr.String())
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope.Data)
	if !envelope.OK || !bytes.Contains(encoded, []byte(`"subdir":"plugins/demo"`)) || !bytes.Contains(encoded, []byte(`"source_identity":"`)) {
		t.Fatalf("subdir confirmation missing: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("ignored=query")) || bytes.Contains(encoded, []byte("ignored-fragment")) {
		t.Fatalf("source URL query/fragment leaked into preview: %s", encoded)
	}
	database, err = store.OpenReadOnly(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	binding, err := database.GetBinding(context.Background(), "binding")
	if err != nil || binding.Managed {
		t.Fatalf("dry-run binding=%#v err=%v", binding, err)
	}
	var intents int
	if err := database.DB().QueryRow(`SELECT count(*) FROM adoption_intents`).Scan(&intents); err != nil || intents != 0 {
		t.Fatalf("dry-run adoption intents=%d err=%v", intents, err)
	}
	if info, err := os.Lstat(installPath); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dry-run changed install path: info=%v err=%v", info, err)
	}
}

func TestHumanWritePromptsExactlyOnce(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader("n\n"))
	command := New(options)
	command.SetArgs([]string{"project", "init"})
	err := command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("expected declined confirmation, got %v", err)
	}
	if count := strings.Count(out.String(), "Apply this plan? [y/N]"); count != 1 {
		t.Fatalf("expected one prompt, got %d\n%s", count, out.String())
	}
	if _, err := os.Stat(filepath.Join(options.WorkingDir, "tooltend.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("declined plan wrote project manifest: %v", err)
	}
}

func TestInitBeforeConfirmationIsStrictlyReadOnly(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	root := options.Getenv("TOOLTEND_HOME")
	command := New(options)
	command.SetArgs([]string{"init", "--json"})
	err := command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("expected confirmation error, got %v\nstderr=%s", err, stderr.String())
	}
	if strings.Contains(out.String(), "export PATH=") {
		t.Fatalf("init preview exposed shell profile content: %s", out.String())
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("init mutated ToolTend root before confirmation: %v", statErr)
	}
	for _, path := range []string{
		filepath.Join(options.HomeDir, ".profile"),
		filepath.Join(options.HomeDir, ".zprofile"),
		filepath.Join(options.HomeDir, ".codex", "hooks.json"),
		filepath.Join(options.HomeDir, ".claude", "settings.json"),
		filepath.Join(options.HomeDir, "Library", "LaunchAgents", "io.tooltend.reconcile.plist"),
		filepath.Join(options.HomeDir, ".config", "systemd", "user", "tooltend-reconcile.service"),
		filepath.Join(options.HomeDir, ".config", "systemd", "user", "tooltend-reconcile.timer"),
	} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("init mutated %s before confirmation: %v", path, statErr)
		}
	}
}

func TestJSONStatusWhenNotInitialized(t *testing.T) {
	var out, stderr bytes.Buffer
	command := New(testOptions(t, &out, &stderr, strings.NewReader("")))
	command.SetArgs([]string{"status", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("status: %v\n%s", err, stderr.String())
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope.Data)
	if !envelope.OK || !bytes.Contains(encoded, []byte(`"initialized":false`)) {
		t.Fatalf("unexpected status: %s", out.String())
	}
}

func TestStatusDoesNotTreatRepairSkeletonAsInitialized(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAtomic(paths.ConfigFile, config.Default()); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	command := New(options)
	command.SetArgs([]string{"status", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope.Data)
	if bytes.Contains(encoded, []byte(`"initialized":true`)) || !bytes.Contains(encoded, []byte("project_selection_missing")) {
		t.Fatalf("repair skeleton reported initialized: %s", encoded)
	}
}

func TestStatusRequiresSelectedProjectInventory(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Projects = []string{options.WorkingDir}
	if err := config.SaveAtomic(paths.ConfigFile, cfg); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	command := New(options)
	command.SetArgs([]string{"status", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope v1.Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope.Data)
	if bytes.Contains(encoded, []byte(`"initialized":true`)) || !bytes.Contains(encoded, []byte("project_inventory_missing")) {
		t.Fatalf("empty project inventory reported initialized: %s", encoded)
	}
}

func TestDoctorRepairDoesNotOverwriteInvalidConfig(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("version = [invalid\n")
	if err := os.WriteFile(paths.ConfigFile, original, 0o600); err != nil {
		t.Fatal(err)
	}
	command := New(options)
	command.SetArgs([]string{"doctor", "--repair", "--yes", "--json"})
	err := command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("doctor unexpectedly replaced invalid config: %v", err)
	}
	current, readErr := os.ReadFile(paths.ConfigFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("invalid config changed: %q", current)
	}
	var envelope v1.Envelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if envelope.Error == nil || envelope.Error.Code != "invalid_configuration" {
		t.Fatalf("unexpected doctor error: %+v", envelope)
	}
}

func TestJSONArgumentErrorsUseStableEnvelope(t *testing.T) {
	var out, stderr bytes.Buffer
	command := New(testOptions(t, &out, &stderr, strings.NewReader("")))
	command.SetArgs([]string{"components", "show", "--json"})
	err := command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("expected reported argument error, got %v", err)
	}
	var envelope v1.Envelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode envelope: %v\n%s", decodeErr, out.String())
	}
	if envelope.OK || envelope.Command != "components show" || envelope.Error == nil || envelope.Error.Code != "invalid_argument" {
		t.Fatalf("unexpected argument envelope: %+v", envelope)
	}
}

func TestParentCommandsRejectUnknownSubcommandsInJSONMode(t *testing.T) {
	for _, parent := range []string{"components", "policy", "project", "self"} {
		t.Run(parent, func(t *testing.T) {
			var out, stderr bytes.Buffer
			command := New(testOptions(t, &out, &stderr, strings.NewReader("")))
			command.SetArgs([]string{parent, "bogus", "--json"})
			err := command.Execute()
			if err == nil || !IsReported(err) {
				t.Fatalf("unknown %s subcommand succeeded: %v", parent, err)
			}
			var envelope v1.Envelope
			if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
				t.Fatalf("decode: %v\n%s", decodeErr, out.String())
			}
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != "invalid_argument" {
				t.Fatalf("unexpected envelope: %+v", envelope)
			}
		})
	}
}

func TestAdoptPreviewSourceRedactsURLCredentials(t *testing.T) {
	for input, want := range map[string]string{
		"https://user:secret@example.test/repo.git?token=secret#fragment":     "https://example.test/repo.git",
		"git:https://user:secret@example.test/repo.git?token=secret#fragment": "git:https://example.test/repo.git",
		"http:https://example.test/mcp?token=secret":                          "http:https://example.test/mcp",
		"git@secret@example.test:org/repo.git":                                "git@example.test:org/repo.git",
	} {
		got := safeSourceForPreview(input)
		if got != want {
			t.Fatalf("redacted source=%q want=%q", got, want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "user") || strings.Contains(got, "token=") {
			t.Fatalf("source preview leaked credentials: %q", got)
		}
	}
}

func TestHookFailsOpenWithoutInitializedDatabase(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(`{"session_id":"s","cwd":"/tmp","hook_event_name":"SessionStart"}`))
	command := New(options)
	command.SetArgs([]string{"hook", "--host", "codex", "--event", "SessionStart"})
	if err := command.Execute(); err != nil {
		t.Fatalf("hook blocked host without a database: %v", err)
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("uninitialized hook emitted output: stdout=%q stderr=%q", out.String(), stderr.String())
	}
	if _, err := os.Stat(options.Getenv("TOOLTEND_HOME")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook created state: %v", err)
	}
}

func TestHookRejectsFlagPayloadEventMismatch(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(`{"session_id":"s","cwd":"/tmp","hook_event_name":"SessionStart"}`))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAtomic(paths.ConfigFile, config.Default()); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	command := New(options)
	command.SetArgs([]string{"hook", "--host", "codex", "--event", "PreToolUse"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	database, err = store.OpenReadOnly(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.DB().QueryRow(`SELECT count(*) FROM hook_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || out.Len() != 0 {
		t.Fatalf("mismatched hook was processed: events=%d output=%q", count, out.String())
	}
}

func TestHookCLIWithSQLiteMeetsP95Budget(t *testing.T) {
	skipHookPerformanceTestWhenRequested(t)

	var seedOut, seedErr bytes.Buffer
	base := testOptions(t, &seedOut, &seedErr, strings.NewReader(""))
	paths := config.ResolveWith(base.HomeDir, base.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	payload := `{"session_id":"s","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"tool","tool_input":{"command":"npm install foo@1.2.3"}}`
	durations := make([]time.Duration, 0, 50)
	for range 50 {
		var out, stderr bytes.Buffer
		options := base
		options.In, options.Out, options.ErrOut = strings.NewReader(payload), &out, &stderr
		started := time.Now()
		command := New(options)
		command.SetArgs([]string{"hook", "--host", "codex", "--event", "PreToolUse"})
		if err := command.Execute(); err != nil {
			t.Fatalf("hook: %v", err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if p95 := durations[47]; p95 >= 50*time.Millisecond {
		t.Fatalf("full hook CLI + SQLite p95 = %s, want < 50ms", p95)
	}
}

func TestHookProcessMeetsP95Budget(t *testing.T) {
	skipHookPerformanceTestWhenRequested(t)

	var seedOut, seedErr bytes.Buffer
	base := testOptions(t, &seedOut, &seedErr, strings.NewReader(""))
	paths := config.ResolveWith(base.HomeDir, base.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"s","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"tool","tool_input":{"command":"npm install foo@1.2.3"}}`
	durations := make([]time.Duration, 0, 30)
	for range 30 {
		var out, stderr bytes.Buffer
		command := stdexec.Command(binary, "-test.run=^TestHookProcessHelper$")
		command.Env = append(os.Environ(),
			"TOOLTEND_TEST_HOOK_PROCESS=1",
			"TOOLTEND_TEST_HOOK_HOME="+base.HomeDir,
			"TOOLTEND_TEST_HOOK_STATE="+base.Getenv("TOOLTEND_HOME"),
		)
		command.Stdin, command.Stdout, command.Stderr = strings.NewReader(payload), &out, &stderr
		started := time.Now()
		if err := command.Run(); err != nil {
			t.Fatalf("hook process: %v\nstdout=%s\nstderr=%s", err, out.String(), stderr.String())
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if p95 := durations[28]; p95 >= 50*time.Millisecond {
		t.Fatalf("hook process p95 = %s, want < 50ms", p95)
	}
}

func skipHookPerformanceTestWhenRequested(t *testing.T) {
	t.Helper()
	if os.Getenv("TOOLTEND_TEST_SKIP_HOOK_PERFORMANCE") == "1" {
		t.Skip("hook performance tests run separately from the shared package suite")
	}
}

func TestHookProcessHelper(t *testing.T) {
	if os.Getenv("TOOLTEND_TEST_HOOK_PROCESS") != "1" {
		return
	}
	home, state := os.Getenv("TOOLTEND_TEST_HOOK_HOME"), os.Getenv("TOOLTEND_TEST_HOOK_STATE")
	options := Options{
		In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr,
		HomeDir: home, WorkingDir: "/tmp", Executable: "/bin/true",
		Getenv: func(key string) string {
			if key == "TOOLTEND_HOME" {
				return state
			}
			return ""
		},
	}
	command := New(options)
	command.SetArgs([]string{"hook", "--host", "codex", "--event", "PreToolUse"})
	if err := command.Execute(); err != nil {
		os.Exit(91)
	}
	os.Exit(0)
}

func TestConcurrentSessionStartQueuesOneWorker(t *testing.T) {
	var seedOut, seedErr bytes.Buffer
	base := testOptions(t, &seedOut, &seedErr, strings.NewReader(""))
	paths := config.ResolveWith(base.HomeDir, base.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAtomic(paths.ConfigFile, config.Default()); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "starts")
	helper := filepath.Join(t.TempDir(), "worker")
	script := []byte("#!/bin/sh\nprintf 'started\\n' >> " + shellSingleQuote(counter) + "\n")
	if err := os.WriteFile(helper, script, 0o700); err != nil {
		t.Fatal(err)
	}
	base.Executable = helper

	const calls = 20
	errorsSeen := make(chan error, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var out, stderr bytes.Buffer
			options := base
			options.In, options.Out, options.ErrOut = strings.NewReader(`{"session_id":"s","cwd":"/tmp","hook_event_name":"SessionStart"}`), &out, &stderr
			command := New(options)
			command.SetArgs([]string{"hook", "--host", "codex", "--event", "SessionStart"})
			errorsSeen <- command.Execute()
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("SessionStart hook: %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		content, readErr := os.ReadFile(counter)
		if readErr == nil && strings.Count(string(content), "started\n") == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached worker starts=%q err=%v, want exactly one", content, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInitAndDryRunDoNotApplyPendingSelfUpdate(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		code string
	}{
		{name: "init", args: []string{"init", "--json"}, code: "confirmation_required"},
		{name: "dry-run", args: []string{"project", "init", "--json", "--dry-run"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			options := testOptions(t, &out, &stderr, strings.NewReader(""))
			root := options.Getenv("TOOLTEND_HOME")
			pendingPath := filepath.Join(root, "self-update", "pending.json")
			if err := os.MkdirAll(filepath.Dir(pendingPath), 0o700); err != nil {
				t.Fatal(err)
			}
			pending := []byte(`{"invalid":true}`)
			if err := os.WriteFile(pendingPath, pending, 0o600); err != nil {
				t.Fatal(err)
			}
			live := filepath.Join(root, "live-tooltend")
			liveContent := []byte("old binary\n")
			if err := os.WriteFile(live, liveContent, 0o700); err != nil {
				t.Fatal(err)
			}
			options.Executable = live
			command := New(options)
			command.SetArgs(test.args)
			err := command.Execute()
			if test.code == "" && err != nil {
				t.Fatalf("execute: %v\n%s", err, out.String())
			}
			var envelope v1.Envelope
			if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
				t.Fatalf("decode: %v\n%s", decodeErr, out.String())
			}
			if test.code != "" && (envelope.Error == nil || envelope.Error.Code != test.code) {
				t.Fatalf("unexpected failure: %+v", envelope)
			}
			if got, readErr := os.ReadFile(live); readErr != nil || !bytes.Equal(got, liveContent) {
				t.Fatalf("live binary changed: %q, %v", got, readErr)
			}
			if got, readErr := os.ReadFile(pendingPath); readErr != nil || !bytes.Equal(got, pending) {
				t.Fatalf("pending metadata changed: %q, %v", got, readErr)
			}
		})
	}
}

func TestSelectedAgentLimitsInitInventory(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	for _, path := range []string{
		filepath.Join(options.HomeDir, ".codex", "skills", "codex-only", "SKILL.md"),
		filepath.Join(options.HomeDir, ".claude", "skills", "claude-only", "SKILL.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nname: fixture\ndescription: fixture\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a := newApp(options)
	report, err := a.scanInventory(context.Background(), []model.HostKind{model.HostCodex}, options.WorkingDir, []string{options.WorkingDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.HostResult.Observations) == 0 {
		t.Fatal("expected Codex observation")
	}
	for _, observation := range report.HostResult.Observations {
		if observation.Host != "codex" {
			t.Fatalf("selected Codex scan included %s: %#v", observation.Host, observation)
		}
	}
}

func TestScanCannotCreatePartiallyInitializedState(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	root := options.Getenv("TOOLTEND_HOME")
	command := New(options)
	command.SetArgs([]string{"scan", "--yes", "--json"})
	err := command.Execute()
	if err == nil || !IsReported(err) {
		t.Fatalf("expected not-initialized error, got %v", err)
	}
	var envelope v1.Envelope
	if decodeErr := json.Unmarshal(out.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode: %v\n%s", decodeErr, out.String())
	}
	if envelope.Error == nil || envelope.Error.Code != "not_initialized" {
		t.Fatalf("unexpected error: %+v", envelope)
	}
	if _, statErr := os.Stat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("scan created a partial ToolTend root: %v", statErr)
	}
}

func TestHumanReviewCannotBeAuthorizedByFlagsOrJSON(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader("yes\n"))
	a := newApp(options)
	a.global.Yes = true
	if a.humanReviewAuthorized() {
		t.Fatal("--yes authorized a human review")
	}
	a.global.Yes, a.global.JSON = false, true
	if a.humanReviewAuthorized() {
		t.Fatal("JSON mode authorized a human review")
	}
}

func TestReviewSubmissionInputIsBoundedAndUsesBundleRisk(t *testing.T) {
	base := reviewOptions{
		CandidateID: "cand_fixture", CandidateHash: strings.Repeat("a", 64), Verdict: "safe",
		RiskType: "hook_content_change", Summary: "reviewed the hook behavior", Actor: "agent",
	}
	if submitting, err := validateReviewOptions(base); err != nil || !submitting {
		t.Fatalf("valid review options: submitting=%v err=%v", submitting, err)
	}
	for name, mutate := range map[string]func(*reviewOptions){
		"invalid risk":  func(value *reviewOptions) { value.RiskType = "Hook Change" },
		"empty summary": func(value *reviewOptions) { value.Summary = "   " },
		"control text":  func(value *reviewOptions) { value.Summary = "first\nsecond" },
		"oversized":     func(value *reviewOptions) { value.Summary = strings.Repeat("a", 1025) },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if _, err := validateReviewOptions(value); err == nil {
				t.Fatal("expected bounded review input validation failure")
			}
		})
	}

	candidate := model.UpdateCandidate{ID: base.CandidateID, CandidateHash: base.CandidateHash}
	bundle := model.ReviewBundle{CandidateID: base.CandidateID, CandidateHash: base.CandidateHash, RiskTypesJSON: `["hook_content_change"]`}
	if err := validateReviewRisk(bundle, candidate, base.RiskType); err != nil {
		t.Fatalf("allowed risk rejected: %v", err)
	}
	if err := validateReviewRisk(bundle, candidate, "permission_change"); err == nil {
		t.Fatal("risk absent from bundle was accepted")
	}
}

func TestShimPlanningUsesWritableHomePATHOrPreviewsProfile(t *testing.T) {
	var out, stderr bytes.Buffer
	base := testOptions(t, &out, &stderr, strings.NewReader(""))
	bin := filepath.Join(base.HomeDir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	root := base.Getenv("TOOLTEND_HOME")
	base.Getenv = func(key string) string {
		switch key {
		case "TOOLTEND_HOME":
			return root
		case "PATH":
			return bin
		case "SHELL":
			return "/bin/zsh"
		default:
			return ""
		}
	}
	a := newApp(base)
	paths := config.ResolveWith(base.HomeDir, base.Getenv)
	shim, mutation, err := a.planShimPath(paths)
	if err != nil {
		t.Fatal(err)
	}
	if shim != bin || mutation != nil {
		t.Fatalf("writable PATH selection: shim=%q mutation=%#v", shim, mutation)
	}

	systemBin := filepath.Join(t.TempDir(), "system-bin")
	if err := os.MkdirAll(systemBin, 0o755); err != nil {
		t.Fatal(err)
	}
	base.Getenv = func(key string) string {
		switch key {
		case "TOOLTEND_HOME":
			return root
		case "PATH":
			return systemBin + string(os.PathListSeparator) + bin
		case "SHELL":
			return "/bin/zsh"
		default:
			return ""
		}
	}
	a = newApp(base)
	paths = config.ResolveWith(base.HomeDir, base.Getenv)
	shim, mutation, err = a.planShimPath(paths)
	if err != nil {
		t.Fatal(err)
	}
	if shim != paths.ShimDir || mutation == nil {
		t.Fatalf("late writable PATH dir was reused: shim=%q mutation=%#v", shim, mutation)
	}

	base.Getenv = func(key string) string {
		switch key {
		case "TOOLTEND_HOME":
			return root
		case "SHELL":
			return "/bin/zsh"
		default:
			return ""
		}
	}
	a = newApp(base)
	paths = config.ResolveWith(base.HomeDir, base.Getenv)
	shim, mutation, err = a.planShimPath(paths)
	if err != nil {
		t.Fatal(err)
	}
	if shim != paths.ShimDir || mutation == nil || mutation.Path != filepath.Join(base.HomeDir, ".zprofile") {
		t.Fatalf("fallback plan: shim=%q mutation=%#v", shim, mutation)
	}
	if !bytes.Contains(mutation.Content, []byte("# ToolTend")) || !bytes.Contains(mutation.Content, []byte(paths.ShimDir)) {
		t.Fatalf("profile mutation lacks marker: %q", mutation.Content)
	}
	if _, statErr := os.Stat(mutation.Path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("planning wrote shell profile: %v", statErr)
	}
}

func TestReconcileDryRunDoesNotInvokeWorker(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	called := false
	options.RunOnce = func(context.Context, config.Paths) (any, error) {
		called = true
		return nil, nil
	}
	command := New(options)
	command.SetArgs([]string{"reconcile", "--once", "--dry-run", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("reconcile dry-run: %v\n%s", err, out.String())
	}
	if called {
		t.Fatal("reconcile --dry-run invoked the worker")
	}
}

func TestPolicyAndProjectMutationHelperHonorsActivationLock(t *testing.T) {
	var out, stderr bytes.Buffer
	options := testOptions(t, &out, &stderr, strings.NewReader(""))
	paths := config.ResolveWith(options.HomeDir, options.Getenv)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := lockfile.Try(paths.ActivationLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	called := false
	err = withLifecycleStateLock(context.Background(), paths, func(*store.Store) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("locked lifecycle mutation err=%v called=%v", err, called)
	}
}

func testOptions(t *testing.T, out, stderr *bytes.Buffer, in *strings.Reader) Options {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	work := filepath.Join(root, "project")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	tooltendRoot := filepath.Join(root, "tooltend-home")
	getenv := func(key string) string {
		if key == "TOOLTEND_HOME" {
			return tooltendRoot
		}
		return ""
	}
	return Options{
		In: in, Out: out, ErrOut: stderr,
		HomeDir: home, WorkingDir: work, Executable: "/bin/true", Getenv: getenv,
	}
}
