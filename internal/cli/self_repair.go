package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	v1 "github.com/z2z23n0/tooltend/internal/api/v1"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/notify"
	"github.com/z2z23n0/tooltend/internal/scheduler"
)

// repairAfterSelfUpdate touches only integrations whose entries are owned by
// ToolTend. Host planners preserve unrelated user hooks and fail closed when
// their structure cannot be inspected safely.
func (a *App) repairAfterSelfUpdate(ctx context.Context, paths config.Paths) []v1.Warning {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return []v1.Warning{{Code: "self_update_repair_skipped", Message: "self-update applied, but integration repair was skipped because configuration is unavailable"}}
	}
	if cfg.Runtime.ShimDir != "" {
		paths.ShimDir = filepath.Clean(cfg.Runtime.ShimDir)
	}
	var warnings []v1.Warning
	plans, err := a.planHooks(ctx, cfg.Agents)
	if err != nil {
		warnings = append(warnings, v1.Warning{Code: "self_update_hook_conflict", Message: "self-update applied, but user hook configuration could not be repaired safely"})
	} else {
		for _, value := range plans {
			for _, mutation := range value.Mutations {
				if mutation.Changed {
					if err := host.ApplyMutation(mutation); err != nil {
						warnings = append(warnings, v1.Warning{Code: "self_update_hook_conflict", Message: "self-update applied, but a hook changed concurrently and was not overwritten"})
					}
				}
			}
		}
	}
	schedule, err := scheduler.BuildPlan(scheduler.Options{Executable: a.executable, Home: a.home, StateDir: paths.StateDir, Hour: -1, Minute: -1})
	if err == nil {
		if err = scheduler.Apply(schedule); err == nil {
			err = scheduler.Activate(ctx, schedule, a.runner)
		}
	}
	if err != nil {
		warnings = append(warnings, v1.Warning{Code: "self_update_scheduler_repair_failed", Message: fmt.Sprintf("self-update applied, but the ToolTend scheduler needs repair: %s", err)})
	}
	if runtime.GOOS == "darwin" {
		if _, err := notify.InstallDarwin(ctx, a.home, a.runner); err != nil {
			warnings = append(warnings, v1.Warning{Code: "self_update_notifier_repair_failed", Message: fmt.Sprintf("self-update applied, but ToolTend Notifier needs repair: %s", err)})
		}
	}
	return warnings
}
