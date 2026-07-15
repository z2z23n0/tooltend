package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/buildinfo"
	"github.com/z2z23n0/tooltend/internal/bundle"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/safeio"
	"github.com/z2z23n0/tooltend/internal/scheduler"
	"github.com/z2z23n0/tooltend/internal/store"
)

type resetBlockers struct {
	ManagedBindings      int `json:"managed_bindings"`
	ManagedInstallations int `json:"managed_installations"`
	ManagedBundles       int `json:"managed_bundles"`
	ActivationJournals   int `json:"activation_journals"`
	AdoptionJournals     int `json:"adoption_journals"`
	BundleTransactions   int `json:"bundle_transactions"`
}

type resetSnapshot struct {
	Source string `json:"source"`
	Backup string `json:"backup"`
}

type resetResult struct {
	Backup      string                  `json:"backup"`
	Inventory   inventory.PersistResult `json:"inventory"`
	Bundles     bundle.DiscoverResult   `json:"bundles"`
	NextCommand string                  `json:"next_command"`
}

func (a *App) resetState(ctx context.Context, paths config.Paths, options initOptions) (mutationResult, error) {
	blockers, schema, err := inspectResetBlockers(ctx, paths.DatabaseFile)
	if err != nil {
		return mutationResult{}, err
	}
	if blockers.ManagedBindings+blockers.ManagedInstallations+blockers.ManagedBundles+blockers.ActivationJournals+blockers.AdoptionJournals+blockers.BundleTransactions > 0 {
		return mutationResult{}, &commandError{Code: "reset_refused", Message: "state reset is unsafe while managed objects or unfinished journals exist", Details: map[string]any{"blockers": blockers}}
	}
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return mutationResult{}, err
	}
	if len(options.Agents) > 0 {
		cfg.Agents, err = parseAgents(options.Agents)
		if err != nil {
			return mutationResult{}, err
		}
	}
	if len(options.Projects) > 0 {
		cfg.Projects, err = a.initProjects(nil, options.Projects)
		if err != nil {
			return mutationResult{}, err
		}
	}
	var profile *profileMutation
	if cfg.Runtime.ShimDir == "" {
		cfg.Runtime.ShimDir, profile, err = a.planShimPath(paths)
		if err != nil {
			return mutationResult{}, err
		}
	}
	paths.ShimDir = cfg.Runtime.ShimDir
	currentProject := ""
	if containsPath(cfg.Projects, a.workingDir) {
		currentProject = a.workingDir
	}
	report, err := a.scanInventory(ctx, cfg.Agents, currentProject, cfg.Projects)
	if err != nil {
		return mutationResult{}, err
	}
	hookPlans, err := a.planHooks(ctx, cfg.Agents)
	if err != nil {
		return mutationResult{}, err
	}
	schedule, err := scheduler.BuildPlan(scheduler.Options{Executable: a.executable, Home: a.home, StateDir: paths.StateDir, Hour: -1, Minute: -1})
	if err != nil {
		return mutationResult{}, err
	}
	backupRoot := filepath.Join(filepath.Dir(paths.StateDir), "tooltend-backups", time.Now().UTC().Format("20060102T150405.000000000Z"))
	snapshots := resetSnapshots(paths, hookPlans, schedule, profile, backupRoot)
	configBeforeHash := fileHashOrEmpty(paths.ConfigFile)
	var completed resetResult
	value := plan.Plan{ID: "init-reset-v2", Title: "Back up and rebuild ToolTend v0.2 state", Operations: []plan.Operation{
		plan.FuncOperation{
			Description: plan.OperationPreview{
				ID: "backup-and-reset", Kind: plan.OperationDatabase, Target: paths.StateDir,
				Summary:    "Pause the scheduler, back up all ToolTend state, rebuild schema v5, and discover unconfigured bundles",
				Reversible: false, RequiresConfirmation: true,
				Details: map[string]string{
					"backup": backupRoot, "schema_before": fmt.Sprint(schema), "schema_after": fmt.Sprint(store.SchemaVersion),
					"snapshots": fmt.Sprint(len(snapshots)), "bundle_policy": "all bundles remain unconfigured",
				},
			},
			ApplyFunc: func(applyCtx context.Context) error {
				result, applyErr := a.executeReset(applyCtx, paths, cfg, report, hookPlans, schedule, profile, backupRoot, snapshots, configBeforeHash)
				if applyErr == nil {
					completed = result
				}
				return applyErr
			},
		},
	}}
	return a.applyPlan(ctx, value, func() any { return completed })
}

func inspectResetBlockers(ctx context.Context, databasePath string) (resetBlockers, int, error) {
	database, err := store.OpenReadOnly(databasePath)
	if err != nil {
		return resetBlockers{}, 0, err
	}
	defer database.Close()
	version, err := database.UserVersion(ctx)
	if err != nil {
		return resetBlockers{}, 0, err
	}
	var value resetBlockers
	queries := []struct {
		target *int
		query  string
	}{
		{&value.ManagedBindings, `SELECT COUNT(*) FROM bindings WHERE managed=1`},
		{&value.ActivationJournals, `SELECT COUNT(*) FROM activation_intents WHERE phase IN ('prepared','pointer_switched')`},
		{&value.AdoptionJournals, `SELECT COUNT(*) FROM adoption_intents WHERE phase IN ('prepared','switched','blocked')`},
	}
	if version >= 5 {
		queries = append(queries,
			struct {
				target *int
				query  string
			}{&value.ManagedInstallations, `SELECT COUNT(*) FROM installations WHERE managed=1`},
			struct {
				target *int
				query  string
			}{&value.ManagedBundles, `SELECT COUNT(*) FROM bundle_policies WHERE mode IN ('auto','manual')`},
			struct {
				target *int
				query  string
			}{&value.BundleTransactions, `SELECT COUNT(*) FROM bundle_transactions WHERE status IN ('prepared','staging','activating','rolling_back')`},
		)
	}
	for _, item := range queries {
		if err := database.DB().QueryRowContext(ctx, item.query).Scan(item.target); err != nil {
			return value, version, err
		}
	}
	return value, version, nil
}

func (a *App) executeReset(ctx context.Context, paths config.Paths, cfg config.Config, report inventory.Report, hookPlans []host.MutationPlan, schedule scheduler.Plan, profile *profileMutation, backupRoot string, snapshots []resetSnapshot, configBeforeHash string) (result resetResult, err error) {
	lock, err := lockfile.Try(paths.ActivationLock)
	if err != nil {
		return result, fmt.Errorf("reset: acquire activation lock: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if fileHashOrEmpty(paths.ConfigFile) != configBeforeHash {
		return result, errors.New("reset: configuration changed after preview")
	}
	blockers, _, err := inspectResetBlockers(ctx, paths.DatabaseFile)
	if err != nil {
		return result, err
	}
	if blockers.ManagedBindings+blockers.ManagedInstallations+blockers.ManagedBundles+blockers.ActivationJournals+blockers.AdoptionJournals+blockers.BundleTransactions > 0 {
		return result, &commandError{Code: "reset_refused", Message: "state changed after preview and reset is no longer safe", Details: map[string]any{"blockers": blockers}}
	}

	deactivated := false
	if deactivateErr := scheduler.Deactivate(ctx, schedule, a.runner); deactivateErr == nil {
		deactivated = true
	} else {
		return result, fmt.Errorf("reset: pause scheduler: %w", deactivateErr)
	}
	backupComplete := false
	rollback := func(cause error) error {
		var restoreErr error
		if backupComplete {
			restoreErr = restoreSnapshots(snapshots, paths.ActivationLock)
		}
		if deactivated {
			activateErr := scheduler.Activate(context.WithoutCancel(ctx), schedule, a.runner)
			restoreErr = errors.Join(restoreErr, activateErr)
		}
		return errors.Join(cause, restoreErr)
	}
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return result, rollback(err)
	}
	for _, snapshot := range snapshots {
		if err := copyPath(snapshot.Source, snapshot.Backup, ""); err != nil {
			return result, rollback(fmt.Errorf("reset: back up %s: %w", snapshot.Source, err))
		}
	}
	manifestData, _ := json.MarshalIndent(map[string]any{"schema": store.SchemaVersion, "created_at": time.Now().UTC(), "snapshots": snapshots}, "", "  ")
	if err := safeio.AtomicWriteFile(filepath.Join(backupRoot, "manifest.json"), append(manifestData, '\n'), 0o600); err != nil {
		return result, rollback(err)
	}
	backupComplete = true
	for _, root := range uniqueResetRoots(paths) {
		if err := clearRoot(root, paths.ActivationLock); err != nil {
			return result, rollback(fmt.Errorf("reset: clear %s: %w", root, err))
		}
	}
	if err := paths.Ensure(); err != nil {
		return result, rollback(err)
	}
	if err := os.MkdirAll(paths.ShimDir, 0o755); err != nil {
		return result, rollback(err)
	}
	if profile != nil {
		if err := safeio.AtomicWriteFile(profile.Path, profile.Content, profile.Mode); err != nil {
			return result, rollback(err)
		}
	}
	if err := config.SaveAtomic(paths.ConfigFile, cfg); err != nil {
		return result, rollback(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		return result, rollback(err)
	}
	result.Inventory, err = inventory.Persist(ctx, database, report)
	if err == nil {
		result.Bundles, err = bundle.Discover(ctx, database, bundle.DiscoverOptions{
			HomeDir: a.home, Executable: a.executable, BuildVersion: buildinfo.Version,
			LocalRecipeDir: filepath.Join(paths.ConfigDir, "bundles.d"),
			LookupPath:     a.lookupPath,
		})
	}
	closeErr := database.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return result, rollback(err)
	}
	for _, hookPlan := range hookPlans {
		for _, mutation := range hookPlan.Mutations {
			if mutation.Changed {
				if err := host.ApplyMutation(mutation); err != nil {
					return result, rollback(err)
				}
			}
		}
	}
	if err := scheduler.Apply(schedule); err != nil {
		return result, rollback(err)
	}
	if err := scheduler.Activate(ctx, schedule, a.runner); err != nil {
		return result, rollback(err)
	}
	deactivated = false
	result.Backup, result.NextCommand = backupRoot, "tooltend bundles configure"
	return result, nil
}

func resetSnapshots(paths config.Paths, hookPlans []host.MutationPlan, schedule scheduler.Plan, profile *profileMutation, backupRoot string) []resetSnapshot {
	var sources []string
	sources = append(sources, uniqueResetRoots(paths)...)
	for _, hookPlan := range hookPlans {
		for _, mutation := range hookPlan.Mutations {
			sources = append(sources, mutation.Path)
		}
	}
	for _, file := range schedule.Files {
		sources = append(sources, file.Path)
	}
	if profile != nil {
		sources = append(sources, profile.Path)
	}
	seen := map[string]struct{}{}
	result := []resetSnapshot{}
	for index, source := range sources {
		source = filepath.Clean(source)
		if _, exists := seen[source]; exists {
			continue
		}
		seen[source] = struct{}{}
		result = append(result, resetSnapshot{Source: source, Backup: filepath.Join(backupRoot, fmt.Sprintf("%03d-%s", index, sanitizeBackupName(source)))})
	}
	return result
}

func uniqueResetRoots(paths config.Paths) []string {
	values := []string{paths.ConfigDir, paths.StateDir, paths.DataDir}
	seen := map[string]struct{}{}
	result := []string{}
	for _, value := range values {
		value = filepath.Clean(value)
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return len(result[i]) > len(result[j]) })
	return result
}

func clearRoot(root, preserved string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if filepath.Clean(path) == filepath.Clean(preserved) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func restoreSnapshots(values []resetSnapshot, preserved string) error {
	var result error
	for _, value := range values {
		if filepath.Clean(value.Source) == filepath.Clean(preserved) {
			continue
		}
		_ = os.RemoveAll(value.Source)
		if _, err := os.Lstat(value.Backup); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyPath(value.Backup, value.Source, preserved); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func copyPath(source, destination, preserved string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			childSource := filepath.Join(source, entry.Name())
			if preserved != "" && filepath.Clean(childSource) == filepath.Clean(preserved) {
				continue
			}
			if err := copyPath(childSource, filepath.Join(destination, entry.Name()), preserved); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported special file %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

func sanitizeBackupName(path string) string {
	path = strings.Trim(filepath.ToSlash(path), "/")
	path = strings.NewReplacer("/", "_", " ", "-").Replace(path)
	if len(path) > 80 {
		path = path[len(path)-80:]
	}
	if path == "" {
		return runtime.GOOS
	}
	return path
}
