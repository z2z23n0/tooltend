package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/buildinfo"
	"github.com/z2z23n0/tooltend/internal/bundle"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/hook"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/kick"
	"github.com/z2z23n0/tooltend/internal/lifecycle"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/notify"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/reconcile"
	"github.com/z2z23n0/tooltend/internal/store"
	"github.com/z2z23n0/tooltend/internal/watchdog"
)

type hookStore struct{ database *store.Store }

func (s hookStore) TryRecord(ctx context.Context, event hook.Event) error {
	host := model.HostKind(event.Host)
	if err := host.Validate(); err != nil {
		return err
	}
	var projectID string
	if event.ProjectHash != "" {
		_ = s.database.DB().QueryRowContext(ctx, `SELECT id FROM projects WHERE root_fingerprint=?`, event.ProjectHash).Scan(&projectID)
	}
	_, err := s.database.RecordHookEvent(ctx, model.HookEvent{
		OccurredAt: event.OccurredAt, Host: host, EventType: event.EventType,
		ProjectID: projectID,
		Installer: event.Installer, PackageIdentity: event.PackageIdentity,
		RequestedVersion: event.RequestedVersion, CorrelationHash: event.CorrelationHash,
	})
	return err
}

func (s hookStore) TakePending(ctx context.Context, limit int) ([]string, error) {
	values, err := s.database.TakeNotifications(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value.Message) != "" {
			result = append(result, value.Message)
			continue
		}
		hash := value.CandidateHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		result = append(result, fmt.Sprintf("%s candidate %s", value.Kind, hash))
	}
	return result, nil
}

func (a *App) newHookCommand() *cobra.Command {
	var hostName, eventName string
	command := &cobra.Command{
		Use: "hook", Short: "Record a coding-agent hook event", Hidden: true,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.global.DryRun {
				return nil
			}
			if hostName != string(model.HostCodex) && hostName != string(model.HostClaude) {
				return nil // fail-open
			}
			switch eventName {
			case "SessionStart", "PreToolUse", "PostToolUse":
			default:
				return nil // fail-open
			}
			paths, err := a.paths()
			if err != nil {
				return nil
			}
			database, err := store.OpenHook(paths.DatabaseFile)
			if err != nil {
				return nil
			}
			defer database.Close()
			payload, readErr := io.ReadAll(io.LimitReader(a.in, hook.MaxInputBytes+1))
			if readErr != nil || len(payload) > hook.MaxInputBytes {
				return nil
			}
			var input hook.Input
			if json.Unmarshal(payload, &input) != nil || input.HookEventName != eventName {
				return nil
			}
			adapter := hookStore{database: database}
			handler := hook.Handler{Host: hostName, Events: adapter, Notifications: adapter}
			_ = handler.Run(cmd.Context(), bytes.NewReader(payload), a.out)
			if eventName == "SessionStart" && a.reconcileDue(cmd.Context(), paths, database) {
				_, _ = kick.Queue(a.executable, paths.StateDir, "reconcile", "--once", "--reason", "kick", "--state-dir", paths.StateDir, "--json")
			}
			return nil
		},
	}
	command.Flags().StringVar(&hostName, "host", "", "hook host: codex or claude")
	command.Flags().StringVar(&eventName, "event", "", "hook event name")
	return command
}

func (a *App) reconcileDue(ctx context.Context, paths config.Paths, database *store.Store) bool {
	interval := 24 * time.Hour
	if cfg, err := config.Load(paths.ConfigFile); err == nil && cfg.Check.Interval > 0 {
		interval = cfg.Check.Interval
	}
	var latest sql.NullString
	if err := database.DB().QueryRowContext(ctx, `SELECT max(finished_at) FROM reconcile_runs WHERE status='succeeded'`).Scan(&latest); err != nil {
		return false
	}
	if !latest.Valid {
		return true
	}
	last, err := time.Parse(time.RFC3339Nano, latest.String)
	return err == nil && time.Since(last) >= interval
}

func (a *App) newKickCommand() *cobra.Command {
	command := &cobra.Command{Use: "kick", Short: "Queue a detached one-shot reconciliation worker", Hidden: true, Args: cobra.NoArgs}
	command.RunE = a.run("kick", func(context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		if a.global.DryRun {
			return map[string]any{"started": false, "dry_run": true, "state_dir": paths.StateDir}, nil
		}
		return kick.Queue(a.executable, paths.StateDir, "reconcile", "--once", "--reason", "kick", "--state-dir", paths.StateDir, "--json")
	})
	return command
}

func (a *App) newReconcileCommand() *cobra.Command {
	var once bool
	var reason string
	command := &cobra.Command{Use: "reconcile", Short: "Run one daemonless reconciliation cycle", Hidden: true, Args: cobra.NoArgs}
	command.Flags().BoolVar(&once, "once", false, "run one cycle and exit")
	command.Flags().StringVar(&reason, "reason", reconcile.ReasonScheduled, "worker trigger reason")
	command.RunE = a.run("reconcile", func(ctx context.Context) (any, error) {
		if !once {
			return nil, cliError("invalid_argument", "reconcile requires --once", nil)
		}
		if reason != reconcile.ReasonScheduled && reason != reconcile.ReasonKick && reason != reconcile.ReasonCommand {
			return nil, cliError("invalid_argument", "reason must be scheduled, kick, or command", nil)
		}
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		if a.runOnce != nil {
			if a.global.DryRun {
				return map[string]any{"dry_run": true, "reason": reason, "state_dir": paths.StateDir}, nil
			}
			return a.runOnce(ctx, paths)
		}
		if a.global.DryRun {
			return map[string]any{"dry_run": true, "reason": reason, "state_dir": paths.StateDir}, nil
		}
		value, runErr := a.reconcileOnce(ctx, paths, reason)
		if reason == reconcile.ReasonScheduled {
			a.notifyScheduledOutcome(ctx, paths, value, runErr)
		}
		return value, runErr
	})
	return command
}

func (a *App) newWatchdogCommand() *cobra.Command {
	var maxAge time.Duration
	command := &cobra.Command{Use: "watchdog", Short: "Alert when scheduled reconciliation does not complete", Hidden: true, Args: cobra.NoArgs}
	command.Flags().DurationVar(&maxAge, "max-age", 2*time.Hour, "maximum age of the latest successful reconciliation")
	command.RunE = a.run("watchdog", func(ctx context.Context) (any, error) {
		if maxAge <= 0 {
			return nil, cliError("invalid_argument", "watchdog max age must be positive", nil)
		}
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		if a.global.DryRun {
			return map[string]any{"dry_run": true, "max_age": maxAge.String(), "state_dir": paths.StateDir}, nil
		}
		desktop := notify.Desktop{Runner: a.runner}
		cfg, err := config.Load(paths.ConfigFile)
		if err != nil {
			_ = desktop.Send(ctx, "ToolTend", "Scheduled update state cannot be checked. Run `tooltend doctor` for details.")
			return nil, err
		}
		database, err := store.OpenRW(paths.DatabaseFile)
		if err != nil {
			_ = desktop.Send(ctx, "ToolTend", "Scheduled update state cannot be opened. Run `tooltend doctor` for details.")
			return nil, err
		}
		defer database.Close()
		return (watchdog.Service{
			Database: database,
			Notifier: desktop,
			Enabled:  cfg.Notify.Mode != model.NotifyNone,
		}).Check(ctx, maxAge)
	})
	return command
}

func (a *App) notifyScheduledOutcome(ctx context.Context, paths config.Paths, value any, runErr error) {
	cfg, cfgErr := config.Load(paths.ConfigFile)
	if cfgErr == nil && cfg.Notify.Mode == model.NotifyNone {
		return
	}
	message := ""
	result, _ := value.(reconcile.RunResult)
	switch {
	case runErr != nil && (result.RunID == "" || result.FailureNotificationQueued || result.Failed > 0):
		message = "Scheduled update failed before completion. Run `tooltend doctor` for details."
	case result.Failed > 0 && result.FailureNotificationQueued:
		message = fmt.Sprintf("Scheduled update failed: %d task(s) failed. Run `tooltend doctor` for details.", result.Failed)
	case cfgErr == nil && cfg.Notify.Mode == model.NotifyAll && result.Succeeded > 0:
		message = fmt.Sprintf("Scheduled update completed: %d task(s) succeeded.", result.Succeeded)
	}
	if message != "" {
		_ = (notify.Desktop{Runner: a.runner}).Send(ctx, "ToolTend", message)
	}
}

func (a *App) reconcileOnce(ctx context.Context, paths config.Paths, reason string) (any, error) {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	if cfg.Runtime.ShimDir != "" {
		if !filepath.IsAbs(cfg.Runtime.ShimDir) {
			return nil, cliError("invalid_configuration", "runtime shim directory must be absolute", nil)
		}
		paths.ShimDir = cfg.Runtime.ShimDir
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		return nil, err
	}
	service, err := lifecycle.New(database, objects, paths)
	if err != nil {
		return nil, err
	}
	// reconcile.Worker owns activation.lock for the complete cycle and runs
	// recovery before invoking these callbacks.
	service.ActivationLockHeld = true
	service.PathEnv = prependPATH(paths.ShimDir, os.Getenv("PATH"))
	coordinator := reconcile.CoordinatorFunc(func(ctx context.Context, request reconcile.Request) (reconcile.Outcome, error) {
		result, updateErr := service.Update(ctx, request.Binding.ComponentID, request.Binding.ID, lifecycle.UpdateOptions{
			Stage: request.Stage, Activate: request.Activate, Reason: request.Reason,
		})
		if updateErr != nil {
			return reconcile.Outcome{}, updateErr
		}
		return reconcile.Outcome{
			CandidateID: result.Candidate.ID, CandidateHash: result.Candidate.CandidateHash,
			ResolvedRef: result.ResolvedRef, Checked: result.Checked,
			Changed: result.Candidate.ID != "", Staged: result.Staged,
			Activated: result.Activated, NeedsReview: result.NeedsReview,
		}, nil
	})
	runtimeAdopter := reconcile.RuntimeAdopterFunc(func(ctx context.Context, binding model.Binding) (reconcile.Outcome, error) {
		components, listErr := database.ListComponents(ctx)
		if listErr != nil {
			return reconcile.Outcome{}, listErr
		}
		var currentComponent model.LogicalComponent
		for _, component := range components {
			if component.ID == binding.ComponentID {
				currentComponent = component
				break
			}
		}
		if currentComponent.SourceID == "" {
			return reconcile.Outcome{}, reconcile.NewCodedError("runtime_component_unavailable", false)
		}
		source, sourceErr := database.GetSource(ctx, currentComponent.SourceID)
		if sourceErr != nil {
			return reconcile.Outcome{}, sourceErr
		}
		if source.PackageName == "" || (source.Kind != model.SourceNPM && source.Kind != model.SourcePyPI) {
			return reconcile.Outcome{}, reconcile.NewCodedError("runtime_source_invalid", false)
		}
		sourceSpec := string(source.Kind) + ":" + source.PackageName
		options := lifecycle.AdoptOptions{
			Source: sourceSpec, Version: binding.ObservedVersion, BindingID: binding.ID,
		}
		if currentComponent.Kind == model.ComponentCLI && filepath.IsAbs(binding.InstallPath) {
			options.Executable = filepath.Base(binding.InstallPath)
		}
		target, resolveErr := service.ResolveAdopt(ctx, binding.ComponentID, options)
		if resolveErr != nil {
			return reconcile.Outcome{}, resolveErr
		}
		options.ExpectedResolvedRef = target.ResolvedRef
		options.ExpectedObservedHash = target.ObservedHash
		options.ExpectedConfigHash = target.ConfigHash
		result, adoptErr := service.Adopt(ctx, binding.ComponentID, options)
		if adoptErr != nil {
			return reconcile.Outcome{}, adoptErr
		}
		return reconcile.Outcome{Changed: true, Activated: true, ResolvedRef: result.Receipt.ToRef}, nil
	})
	worker := reconcile.Worker{
		Database: database, Paths: paths, Config: cfg, HomeDir: a.home,
		Coordinator: coordinator, RuntimeAdopter: runtimeAdopter,
		Inventory: func(scanCtx context.Context, scanDB *store.Store, _ reconcile.InventoryOptions) (inventory.PersistResult, error) {
			report, scanErr := a.scanInventory(scanCtx, cfg.Agents, "", cfg.Projects)
			if scanErr != nil {
				return inventory.PersistResult{}, scanErr
			}
			return inventory.Persist(scanCtx, scanDB, report)
		},
		BundleInventory: func(scanCtx context.Context, scanDB *store.Store) (bundle.DiscoverResult, error) {
			return bundle.Discover(scanCtx, scanDB, bundle.DiscoverOptions{
				HomeDir: a.home, Executable: a.executable, BuildVersion: buildinfo.Version,
				LocalRecipeDir: filepath.Join(paths.ConfigDir, "bundles.d"),
				LookupPath:     a.lookupPath,
			})
		},
		BundleRecovery: func(recoveryCtx context.Context) (bundle.RecoveryResult, error) {
			bundleService := bundle.Service{Database: database, Paths: paths, Runner: a.runner}
			return bundleService.RecoverTransactions(recoveryCtx)
		},
		BundleCoordinator: reconcile.BundleCoordinatorFunc(func(bundleCtx context.Context, value model.Bundle, _ model.BundlePolicy, activate bool) error {
			bundleService := bundle.Service{Database: database, Paths: paths, Runner: a.runner}
			preview, prepareErr := bundleService.PrepareUpdate(bundleCtx, value.ID, false)
			if prepareErr != nil {
				return prepareErr
			}
			if !activate {
				return database.UpsertBundleRelease(bundleCtx, preview.Target)
			}
			_, executeErr := bundleService.ExecuteUpdate(bundleCtx, preview)
			return executeErr
		}),
	}
	return worker.RunOnce(ctx, reason)
}

func prependPATH(directory, current string) string {
	directory = filepath.Clean(directory)
	entries := []string{directory}
	for _, entry := range filepath.SplitList(current) {
		if filepath.Clean(entry) != directory {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(os.PathListSeparator))
}
