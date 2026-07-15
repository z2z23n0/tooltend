package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/buildinfo"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/doctor"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/selfupdate"
)

func (a *App) selfUpdateManager(stateDir, manifestURL string) selfupdate.Manager {
	return selfupdate.Manager{
		StateDir: stateDir, Executable: a.executable, ManifestURL: manifestURL,
		CurrentVersion: buildinfo.Version, CurrentSequence: buildinfo.ReleaseSequence(),
	}
}

func (a *App) newSelfCommand() *cobra.Command {
	parent := &cobra.Command{Use: "self", Short: "Inspect or stage a verified ToolTend release", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	status := &cobra.Command{Use: "status", Short: "Show install method and pending self-update", Args: cobra.NoArgs}
	status.RunE = a.run("self status", func(context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		value, err := a.selfUpdateManager(paths.StateDir, "").Status()
		if err != nil {
			return nil, err
		}
		result := map[string]any{"build": buildinfo.Current(), "update": value, "signature": selfupdate.EmbeddedSignatureCapability()}
		if a.selfApply.Applied {
			result["applied_before_command"] = a.selfApply
		}
		return result, nil
	})

	var manifestURL string
	update := &cobra.Command{Use: "update", Short: "Verify and stage the latest signed ToolTend release", Args: cobra.NoArgs}
	update.Flags().StringVar(&manifestURL, "manifest-url", selfupdate.DefaultManifestURL, "signed release manifest URL")
	update.RunE = a.run("self update", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		manager := a.selfUpdateManager(paths.StateDir, manifestURL)
		status, err := manager.Status()
		if err != nil {
			return nil, err
		}
		if status.InstallMethod == "homebrew" {
			return nil, &commandError{Code: "homebrew_managed", Message: selfupdate.ErrHomebrewManaged.Error(), Cause: selfupdate.ErrHomebrewManaged}
		}
		prepared, err := manager.Prepare(ctx)
		if err != nil {
			return nil, err
		}
		verified := prepared.Verified
		var pending selfupdate.Pending
		value := plan.Plan{ID: "self-update-v1", Title: "Stage a signed ToolTend release", Operations: []plan.Operation{
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "stage-self-update", Kind: plan.OperationStageRuntime, Target: paths.StateDir,
					Summary:   "Download the platform binary, verify its signed size and SHA-256, and stage it for the next invocation",
					AfterHash: verified.Asset.SHA256, RequiresConfirmation: true,
					Details: map[string]string{"version": verified.Manifest.Version, "sequence": fmt.Sprint(verified.Manifest.Sequence), "asset_url": safeSourceForPreview(verified.Asset.URL)},
				},
				ApplyFunc: func(ctx context.Context) error {
					var stageErr error
					pending, stageErr = manager.StagePrepared(ctx, prepared)
					return stageErr
				},
			},
		}}
		return a.applyPlan(ctx, value, func() any { return pending })
	})
	parent.AddCommand(status, update)
	return parent
}

func (a *App) newDoctorCommand() *cobra.Command {
	var repair bool
	command := &cobra.Command{Use: "doctor", Short: "Check ToolTend state and repair safe local prerequisites", Args: cobra.NoArgs}
	command.Flags().BoolVar(&repair, "repair", false, "preview and apply repairable local state changes")
	command.RunE = a.run("doctor", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		cfg := config.Default()
		loaded, loadErr := config.Load(paths.ConfigFile)
		if loadErr == nil {
			cfg = loaded
		} else if repair && !errors.Is(loadErr, os.ErrNotExist) {
			return nil, cliError("invalid_configuration", "doctor --repair refuses to replace an existing unreadable or invalid configuration", loadErr)
		}
		if cfg.Runtime.ShimDir != "" {
			if !filepath.IsAbs(cfg.Runtime.ShimDir) {
				return nil, cliError("invalid_configuration", "runtime shim directory must be absolute", nil)
			}
			paths.ShimDir = filepath.Clean(cfg.Runtime.ShimDir)
		}
		options := doctor.Options{Paths: paths, Home: a.home, Executable: a.executable, Agents: cfg.Agents, Config: cfg, Runner: a.runner}
		report := doctor.RunWithOptions(ctx, options)
		if !repair {
			return report, nil
		}
		repairPlan, err := doctor.RepairPlanWithOptions(ctx, options)
		if err != nil {
			return nil, err
		}
		return a.applyPlan(ctx, repairPlan, func() any {
			return map[string]any{"before": report, "after": doctor.RunWithOptions(ctx, options)}
		})
	})
	return command
}

func (a *App) newVersionCommand() *cobra.Command {
	command := &cobra.Command{Use: "version", Short: "Show build version and platform", Args: cobra.NoArgs, Hidden: true}
	command.RunE = a.run("version", func(context.Context) (any, error) { return buildinfo.Current(), nil })
	return command
}

func homebrewError(err error) error {
	if errors.Is(err, selfupdate.ErrHomebrewManaged) {
		return &commandError{Code: "homebrew_managed", Message: selfupdate.ErrHomebrewManaged.Error(), Cause: err}
	}
	return err
}
