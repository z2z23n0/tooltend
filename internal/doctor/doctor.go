package doctor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/lifecycle"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/scheduler"
	"github.com/z2z23n0/tooltend/internal/store"
)

type Level string

const (
	LevelOK      Level = "ok"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
)

type Check struct {
	Name       string `json:"name"`
	Level      Level  `json:"level"`
	Message    string `json:"message"`
	Repairable bool   `json:"repairable"`
}

type Report struct {
	Healthy bool    `json:"healthy"`
	Checks  []Check `json:"checks"`
}

type Options struct {
	Paths      config.Paths
	Home       string
	Executable string
	Agents     []model.HostKind
	Config     config.Config
	Runner     execx.Runner
}

func Run(ctx context.Context, paths config.Paths) Report {
	report := Report{Healthy: true, Checks: []Check{}}
	appendCheck := func(check Check) {
		report.Checks = append(report.Checks, check)
		if check.Level == LevelError {
			report.Healthy = false
		}
	}

	if value, err := config.Load(paths.ConfigFile); err != nil {
		level := LevelError
		if errors.Is(err, os.ErrNotExist) {
			level = LevelWarning
		}
		appendCheck(Check{Name: "config", Level: level, Message: safeMessage("configuration unavailable", err), Repairable: true})
	} else {
		appendCheck(Check{Name: "config", Level: LevelOK, Message: fmt.Sprintf("configuration version %d is valid", value.Version)})
	}

	if _, err := os.Stat(paths.DatabaseFile); err != nil {
		appendCheck(Check{Name: "database", Level: LevelWarning, Message: "state database is not initialized", Repairable: true})
	} else if database, openErr := store.OpenReadOnly(paths.DatabaseFile); openErr != nil {
		appendCheck(Check{Name: "database", Level: LevelError, Message: "state database cannot be opened", Repairable: false})
	} else {
		defer database.Close()
		version, versionErr := database.UserVersion(ctx)
		var result string
		if versionErr != nil {
			appendCheck(Check{Name: "database", Level: LevelError, Message: "state database schema cannot be read", Repairable: false})
		} else if version > store.SchemaVersion {
			appendCheck(Check{Name: "database", Level: LevelError, Message: "state database is newer than this ToolTend build", Repairable: false})
		} else if version < store.SchemaVersion {
			appendCheck(Check{Name: "database", Level: LevelWarning, Message: fmt.Sprintf("state database schema %d needs migration to %d", version, store.SchemaVersion), Repairable: true})
		} else if err := database.DB().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
			appendCheck(Check{Name: "database", Level: LevelError, Message: "state database integrity check failed", Repairable: false})
		} else {
			appendCheck(Check{Name: "database", Level: LevelOK, Message: "state database is healthy"})
		}
		if version == store.SchemaVersion {
			activations, activationErr := database.ListUnfinishedActivations(ctx)
			adoptions, adoptionErr := database.ListPendingAdoptions(ctx)
			bundleTransactions, bundleErr := database.ListUnfinishedBundleTransactions(ctx)
			switch {
			case activationErr != nil || adoptionErr != nil || bundleErr != nil:
				appendCheck(Check{Name: "lifecycle_journal", Level: LevelError, Message: "lifecycle recovery journals cannot be inspected", Repairable: false})
			default:
				blocked := false
				for _, intent := range adoptions {
					blocked = blocked || intent.Phase == store.AdoptionBlocked
				}
				if blocked {
					appendCheck(Check{Name: "lifecycle_journal", Level: LevelError, Message: "an adoption recovery is blocked by externally changed state", Repairable: false})
				} else if len(activations)+len(adoptions)+len(bundleTransactions) != 0 {
					appendCheck(Check{Name: "lifecycle_journal", Level: LevelWarning, Message: "unfinished lifecycle operations are waiting for recovery", Repairable: true})
				} else {
					appendCheck(Check{Name: "lifecycle_journal", Level: LevelOK, Message: "lifecycle recovery journals are clear"})
				}
			}
			counts, countErr := database.BundleCounts(ctx)
			switch {
			case countErr != nil:
				appendCheck(Check{Name: "bundle_coverage", Level: LevelError, Message: "bundle management coverage cannot be inspected", Repairable: false})
			case counts.Total == 0:
				appendCheck(Check{Name: "bundle_coverage", Level: LevelWarning, Message: "bundle discovery has not been persisted; run tooltend scan", Repairable: true})
			case counts.Configured == 0:
				appendCheck(Check{Name: "bundle_coverage", Level: LevelWarning, Message: fmt.Sprintf("infrastructure is healthy and %d bundles were discovered, but management coverage is zero; run tooltend bundles configure", counts.Total)})
			default:
				appendCheck(Check{Name: "bundle_coverage", Level: LevelOK, Message: fmt.Sprintf("%d of %d bundles are configured", counts.Configured, counts.Total)})
			}
		}
	}

	for name, path := range map[string]string{"objects": paths.ObjectsDir, "staging": paths.StagingDir, "generations": paths.GenerationsDir} {
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			appendCheck(Check{Name: name, Level: LevelWarning, Message: name + " directory is missing", Repairable: true})
		case err != nil || !info.IsDir():
			appendCheck(Check{Name: name, Level: LevelError, Message: name + " path is invalid", Repairable: false})
		case info.Mode().Perm()&0o077 != 0:
			appendCheck(Check{Name: name, Level: LevelWarning, Message: name + " directory permissions are broader than 0700", Repairable: true})
		default:
			appendCheck(Check{Name: name, Level: LevelOK, Message: name + " directory is ready"})
		}
	}

	home, _ := os.UserHomeDir()
	appendCheck(checkScheduler(paths, home))
	return report
}

// RunWithOptions includes host hook checks that require knowing the exact
// installed executable. The legacy Run remains useful for read-only state
// checks in callers that do not have that context.
func RunWithOptions(ctx context.Context, options Options) Report {
	report := Run(ctx, options.Paths)
	if options.Home != "" {
		for index := range report.Checks {
			if report.Checks[index].Name == "scheduler" {
				report.Checks[index] = checkSchedulerWithRunner(ctx, options.Paths, options.Home, options.Executable, options.Runner)
				break
			}
		}
	}
	if options.Home == "" || options.Executable == "" {
		return report
	}
	agents := options.Agents
	if len(agents) == 0 {
		agents = []model.HostKind{model.HostCodex, model.HostClaude}
	}
	for _, agent := range agents {
		var adapter host.Host
		switch agent {
		case model.HostCodex:
			adapter = host.NewCodex()
		case model.HostClaude:
			adapter = host.NewClaude()
		default:
			continue
		}
		planned, err := adapter.PlanHookInstall(ctx, host.HookInstallOptions{HomeDir: options.Home, BinaryPath: options.Executable, Scope: host.ScopeUser})
		name := string(agent) + "_hooks"
		check := Check{Name: name}
		switch {
		case err != nil:
			check.Level, check.Message = LevelError, "hook configuration cannot be inspected safely"
		case mutationChanged(planned):
			check.Level, check.Message, check.Repairable = LevelWarning, "ToolTend observation hooks are missing or stale", true
		default:
			check.Level, check.Message = LevelOK, "ToolTend observation hooks are installed"
		}
		report.Checks = append(report.Checks, check)
		if check.Level == LevelError {
			report.Healthy = false
		}
	}
	report.Healthy = true
	for _, check := range report.Checks {
		if check.Level == LevelError {
			report.Healthy = false
			break
		}
	}
	return report
}

func checkScheduler(paths config.Paths, home string) Check {
	files := schedulerPaths(paths, home)
	for _, schedulerPath := range files {
		info, err := os.Stat(schedulerPath)
		if err != nil || !info.Mode().IsRegular() {
			return Check{Name: "scheduler", Level: LevelWarning, Message: "daily one-shot schedule is not installed", Repairable: true}
		}
		if info.Mode().Perm()&0o077 != 0 {
			return Check{Name: "scheduler", Level: LevelWarning, Message: "daily one-shot schedule permissions are too broad", Repairable: true}
		}
	}
	if len(files) == 0 {
		return Check{Name: "scheduler", Level: LevelWarning, Message: "daily one-shot schedule is not installed", Repairable: true}
	}
	return Check{Name: "scheduler", Level: LevelOK, Message: "daily one-shot schedule is installed"}
}

func checkSchedulerWithRunner(ctx context.Context, paths config.Paths, home, executable string, runner execx.Runner) Check {
	check := checkScheduler(paths, home)
	if check.Level != LevelOK || executable == "" {
		return check
	}
	if !schedulerFilesMatch(paths, home, executable) {
		return Check{Name: "scheduler", Level: LevelWarning, Message: "daily one-shot schedule is stale", Repairable: true}
	}
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	var err error
	switch runtime.GOOS {
	case "darwin":
		_, err = runner.Run(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/io.tooltend.reconcile", os.Getuid()))
	case "linux":
		_, err = runner.Run(ctx, "systemctl", "--user", "is-enabled", "tooltend-reconcile.timer")
	default:
		return check
	}
	if err != nil {
		return Check{Name: "scheduler", Level: LevelWarning, Message: "daily one-shot schedule is not registered", Repairable: true}
	}
	return check
}

func schedulerFilesMatch(paths config.Paths, home, executable string) bool {
	files := schedulerPaths(paths, home)
	if len(files) == 0 {
		return false
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return false
		}
		content, err := os.ReadFile(path)
		if err != nil || !schedulerFileContentMatches(filepath.Base(path), string(content), executable, paths.StateDir) {
			return false
		}
	}
	return true
}

func schedulerFileContentMatches(name, content, executable, stateDir string) bool {
	containsAll := func(values ...string) bool {
		for _, value := range values {
			if !strings.Contains(content, value) {
				return false
			}
		}
		return true
	}
	switch name {
	case "io.tooltend.reconcile.plist":
		return containsAll("reconcile", "--once", "--state-dir", filepath.Base(executable), filepath.Base(stateDir), "<key>PATH</key>")
	case "tooltend-reconcile.service":
		return containsAll("reconcile", "--once", "--state-dir", filepath.Base(executable), filepath.Base(stateDir), `Environment="PATH=`)
	case "tooltend-reconcile.timer":
		return containsAll("[Timer]", "OnCalendar=*-*-* ", "RandomizedDelaySec=1h", "Persistent=true", "[Install]", "WantedBy=timers.target")
	default:
		return false
	}
}

func schedulerPaths(paths config.Paths, home string) []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "LaunchAgents", "io.tooltend.reconcile.plist")}
	case "linux":
		root := filepath.Join(home, ".config", "systemd", "user")
		return []string{filepath.Join(root, "tooltend-reconcile.service"), filepath.Join(root, "tooltend-reconcile.timer")}
	default:
		return nil
	}
}

func RepairPlan(paths config.Paths) plan.Plan {
	return plan.Plan{
		ID:    "doctor-repair-v1",
		Title: "Repair ToolTend local state",
		Operations: []plan.Operation{
			plan.FuncOperation{
				Description: plan.OperationPreview{ID: "ensure-directories", Kind: plan.OperationCreateDirectory, Target: paths.DataDir, Summary: "Create and restrict ToolTend-owned directories", Reversible: false, RequiresConfirmation: true},
				ApplyFunc: func(context.Context) error {
					if err := paths.Ensure(); err != nil {
						return err
					}
					for _, dir := range []string{paths.ConfigDir, paths.StateDir, paths.DataDir, paths.ObjectsDir, paths.StagingDir, paths.GenerationsDir, paths.RuntimesDir} {
						if err := os.Chmod(dir, 0o700); err != nil {
							return err
						}
					}
					return nil
				},
			},
			plan.FuncOperation{
				Description: plan.OperationPreview{ID: "ensure-database", Kind: plan.OperationDatabase, Target: paths.DatabaseFile, Summary: "Create or migrate the state database with a pre-migration backup", Reversible: false, RequiresConfirmation: true},
				ApplyFunc: func(context.Context) error {
					database, err := store.OpenRW(paths.DatabaseFile)
					if err != nil {
						return err
					}
					return database.Close()
				},
			},
			plan.FuncOperation{
				Description: plan.OperationPreview{ID: "recover-lifecycle-journals", Kind: plan.OperationDatabase, Target: paths.DatabaseFile, Summary: "Recover safe pending adoption and activation journals", Reversible: false, RequiresConfirmation: true},
				ApplyFunc: func(ctx context.Context) (err error) {
					lock, err := lockfile.Try(paths.ActivationLock)
					if err != nil {
						return err
					}
					defer func() { err = errors.Join(err, lock.Close()) }()
					database, err := store.OpenRW(paths.DatabaseFile)
					if err != nil {
						return err
					}
					defer func() { err = errors.Join(err, database.Close()) }()
					if _, err := lifecycle.RecoverAdoptions(ctx, database, paths); err != nil {
						return err
					}
					_, err = lifecycle.RecoverActivations(ctx, database, paths)
					return err
				},
			},
		},
	}
}

// RepairPlanWithOptions repairs every item RunWithOptions marks repairable:
// local config, state/database, user hooks, and the registered daily one-shot
// schedule. Planning is read-only; every mutation remains confirmation-bound.
func RepairPlanWithOptions(ctx context.Context, options Options) (plan.Plan, error) {
	result := RepairPlan(options.Paths)
	result.ID = "doctor-repair-v1-full"
	result.Title = "Repair ToolTend configuration and local integrations"

	desired := options.Config
	if desired.Version == 0 {
		desired = config.Default()
	}
	if desired.Runtime.ShimDir == "" {
		desired.Runtime.ShimDir = options.Paths.ShimDir
	}
	if _, err := config.Load(options.Paths.ConfigFile); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return plan.Plan{}, errors.New("doctor: refusing to replace an existing invalid configuration")
		}
		configSnapshot, snapshotErr := snapshotFile(options.Paths.ConfigFile)
		if snapshotErr != nil {
			return plan.Plan{}, snapshotErr
		}
		result.Operations = append(result.Operations, plan.FuncOperation{
			Description: plan.OperationPreview{ID: "repair-config", Kind: plan.OperationWriteFile, Target: options.Paths.ConfigFile, Summary: "Write a valid ToolTend configuration", BeforeHash: configSnapshot.hash, Reversible: false, RequiresConfirmation: true},
			ApplyFunc: func(context.Context) error {
				if err := configSnapshot.verify(); err != nil {
					return err
				}
				return config.SaveAtomic(options.Paths.ConfigFile, desired)
			},
		})
	}

	if options.Home != "" && options.Executable != "" {
		agents := options.Agents
		if len(agents) == 0 {
			agents = []model.HostKind{model.HostCodex, model.HostClaude}
		}
		for _, agent := range agents {
			var adapter host.Host
			switch agent {
			case model.HostCodex:
				adapter = host.NewCodex()
			case model.HostClaude:
				adapter = host.NewClaude()
			default:
				continue
			}
			planned, err := adapter.PlanHookInstall(ctx, host.HookInstallOptions{HomeDir: options.Home, BinaryPath: options.Executable, Scope: host.ScopeUser})
			if err != nil {
				return plan.Plan{}, err
			}
			for index := range planned.Mutations {
				mutation := planned.Mutations[index]
				if !mutation.Changed {
					continue
				}
				operationID := "repair-" + string(agent) + "-hooks"
				result.Operations = append(result.Operations, plan.FuncOperation{
					Description: plan.OperationPreview{ID: operationID, Kind: plan.OperationInstallHook, Target: mutation.Path, Summary: "Install or refresh ToolTend " + string(agent) + " hooks", BeforeHash: mutation.BeforeSHA256, AfterHash: mutation.AfterSHA256, Reversible: false, RequiresConfirmation: true},
					ApplyFunc:   func(context.Context) error { return host.ApplyMutation(mutation) },
				})
			}
		}

		schedulerCheck := checkSchedulerWithRunner(ctx, options.Paths, options.Home, options.Executable, options.Runner)
		if schedulerCheck.Level == LevelOK {
			return result, nil
		}
		writeSchedule := !schedulerFilesMatch(options.Paths, options.Home, options.Executable)
		schedule, err := scheduler.BuildPlan(scheduler.Options{Executable: options.Executable, Home: options.Home, StateDir: options.Paths.StateDir, Hour: -1, Minute: -1})
		if err != nil {
			return plan.Plan{}, err
		}
		target := "daily one-shot schedule"
		if len(schedule.Files) > 0 {
			target = schedule.Files[0].Path
		}
		snapshots := make([]fileSnapshot, 0, len(schedule.Files))
		for _, file := range schedule.Files {
			snapshot, snapshotErr := snapshotFile(file.Path)
			if snapshotErr != nil {
				return plan.Plan{}, snapshotErr
			}
			snapshots = append(snapshots, snapshot)
		}
		summary := "Register the existing daily ToolTend one-shot schedule"
		if writeSchedule {
			summary = "Write and register the daily ToolTend one-shot schedule"
		}
		result.Operations = append(result.Operations, plan.FuncOperation{
			Description: plan.OperationPreview{ID: "repair-scheduler", Kind: plan.OperationInstallSchedule, Target: target, Summary: summary, Reversible: false, RequiresConfirmation: true},
			ApplyFunc: func(applyCtx context.Context) error {
				for _, snapshot := range snapshots {
					if err := snapshot.verify(); err != nil {
						return err
					}
				}
				if writeSchedule {
					if err := scheduler.Apply(schedule); err != nil {
						return err
					}
				}
				return scheduler.Activate(applyCtx, schedule, options.Runner)
			},
		})
	}
	return result, nil
}

type fileSnapshot struct {
	path   string
	hash   string
	exists bool
}

func snapshotFile(path string) (fileSnapshot, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	digest := sha256.Sum256(content)
	return fileSnapshot{path: path, hash: hex.EncodeToString(digest[:]), exists: true}, nil
}

func (s fileSnapshot) verify() error {
	current, err := snapshotFile(s.path)
	if err != nil {
		return err
	}
	if current.exists != s.exists || current.hash != s.hash {
		return fmt.Errorf("doctor: target changed after preview: %s", s.path)
	}
	return nil
}

func mutationChanged(value host.MutationPlan) bool {
	for _, mutation := range value.Mutations {
		if mutation.Changed {
			return true
		}
	}
	return false
}

func safeMessage(prefix string, err error) string {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
		return prefix
	}
	return prefix
}
