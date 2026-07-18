package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/bundle"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/store"
)

type bundleSummary struct {
	Bundle         model.Bundle         `json:"bundle"`
	Policy         *model.BundlePolicy  `json:"policy,omitempty"`
	Artifacts      int                  `json:"artifacts"`
	Installations  int                  `json:"installations"`
	Consumers      int                  `json:"consumer_bindings"`
	CurrentRelease *model.BundleRelease `json:"current_release,omitempty"`
	Status         string               `json:"status"`
}

type bundleDetail struct {
	Bundle         model.Bundle              `json:"bundle"`
	Policy         *model.BundlePolicy       `json:"policy,omitempty"`
	CurrentRelease *model.BundleRelease      `json:"current_release,omitempty"`
	Artifacts      []bundleArtifactDetail    `json:"artifacts"`
	History        []model.BundleReceipt     `json:"history,omitempty"`
	Health         []model.BundleHealthCheck `json:"health,omitempty"`
}

type bundleArtifactDetail struct {
	Artifact      model.BundleArtifact       `json:"artifact"`
	Installations []bundleInstallationDetail `json:"installations"`
}

type bundleInstallationDetail struct {
	Installation model.Installation      `json:"installation"`
	Consumers    []model.ConsumerBinding `json:"consumers"`
}

func (a *App) newBundlesCommand() *cobra.Command {
	parent := &cobra.Command{Use: "bundles", Short: "Manage complete tool bundles", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	parent.AddCommand(
		a.newBundlesListCommand(),
		a.newBundlesShowCommand(),
		a.newBundlesConfigureCommand(),
		a.newBundlesUpdateCommand(),
		a.newBundlesRollbackCommand(),
		a.newBundlesHistoryCommand(),
		a.newBundlesDoctorCommand(),
	)
	return parent
}

func (a *App) newBundlesListCommand() *cobra.Command {
	var all bool
	command := &cobra.Command{Use: "list", Short: "List discovered bundles", Args: cobra.NoArgs}
	command.Flags().BoolVar(&all, "all", false, "include unresolved and host-owned fallback bundles")
	command.RunE = a.run("bundles list", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		database, err := a.openReadOnly(paths)
		if err != nil {
			return nil, err
		}
		defer database.Close()
		bundles, err := database.ListBundles(ctx)
		if err != nil {
			return nil, err
		}
		result := []bundleSummary{}
		for _, value := range bundles {
			if !all && value.RecipeSource == "fallback" && (value.Owner == model.LifecycleHostOwned || value.Owner == model.LifecycleUnresolved) {
				continue
			}
			item, err := loadBundleSummary(ctx, database, value)
			if err != nil {
				return nil, err
			}
			result = append(result, item)
		}
		return result, nil
	})
	return command
}

func (a *App) newBundlesShowCommand() *cobra.Command {
	command := &cobra.Command{Use: "show <bundle>", Short: "Show a bundle, artifacts, and physical installations", Args: cobra.ExactArgs(1)}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("bundles show", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			defer database.Close()
			value, err := resolveBundle(ctx, database, args[0])
			if err != nil {
				return nil, err
			}
			return loadBundleDetail(ctx, database, value)
		})(cmd, args)
	}
	return command
}

func (a *App) newBundlesConfigureCommand() *cobra.Command {
	var settings, trusted []string
	command := &cobra.Command{Use: "configure", Short: "Choose lifecycle policy for discovered bundles", Args: cobra.NoArgs}
	command.Flags().StringSliceVar(&settings, "set", nil, "set <bundle>=auto|manual|observe|ignore (repeatable)")
	command.Flags().StringSliceVar(&trusted, "trust-local", nil, "explicitly trust a local recipe (repeatable)")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		return a.run("bundles configure", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			bundles, err := database.ListBundles(ctx)
			if err != nil {
				database.Close()
				return nil, err
			}
			bySelector := map[string]model.Bundle{}
			for _, value := range bundles {
				bySelector[value.ID], bySelector[strings.ToLower(value.Slug)], bySelector[strings.ToLower(value.Name)] = value, value, value
			}
			if len(settings) == 0 {
				if a.global.JSON {
					database.Close()
					return nil, cliError("invalid_argument", "JSON mode requires at least one --set", nil)
				}
				settings, err = a.interactiveBundleSettings(ctx, database, bundles)
				if err != nil {
					database.Close()
					return nil, err
				}
			}
			trustSet := map[string]struct{}{}
			for _, selector := range trusted {
				value, ok := bySelector[strings.ToLower(strings.TrimSpace(selector))]
				if !ok {
					database.Close()
					return nil, cliError("not_found", fmt.Sprintf("local recipe bundle %q was not found", selector), nil)
				}
				trustSet[value.ID] = struct{}{}
			}
			type desiredPolicy struct {
				bundle model.Bundle
				policy model.BundlePolicy
			}
			var desired []desiredPolicy
			seen := map[string]struct{}{}
			for _, setting := range settings {
				selector, modeText, ok := strings.Cut(setting, "=")
				if !ok || strings.TrimSpace(selector) == "" || strings.TrimSpace(modeText) == "" {
					database.Close()
					return nil, cliError("invalid_argument", fmt.Sprintf("invalid --set %q; expected <bundle>=<mode>", setting), nil)
				}
				value, ok := bySelector[strings.ToLower(strings.TrimSpace(selector))]
				if !ok {
					database.Close()
					return nil, cliError("not_found", fmt.Sprintf("bundle %q was not found", selector), nil)
				}
				if _, duplicate := seen[value.ID]; duplicate {
					database.Close()
					return nil, cliError("invalid_argument", fmt.Sprintf("bundle %q was configured more than once", selector), nil)
				}
				seen[value.ID] = struct{}{}
				mode := model.BundlePolicyMode(strings.ToLower(strings.TrimSpace(modeText)))
				if err := mode.Validate(); err != nil {
					database.Close()
					return nil, cliError("invalid_argument", err.Error(), err)
				}
				if err := validateBundleMode(ctx, database, value, mode); err != nil {
					database.Close()
					return nil, cliError("unsafe_policy", err.Error(), err)
				}
				_, trustRequested := trustSet[value.ID]
				existing, getErr := database.GetBundlePolicy(ctx, value.ID)
				alreadyTrusted := getErr == nil && existing.RecipeTrusted
				if value.RecipeSource == "local" && mode != model.BundlePolicyIgnore && !trustRequested && !alreadyTrusted {
					database.Close()
					return nil, cliError("recipe_trust_required", fmt.Sprintf("local recipe for %s requires --trust-local %s", value.Name, value.Slug), nil)
				}
				desired = append(desired, desiredPolicy{bundle: value, policy: model.BundlePolicy{
					BundleID: value.ID, Mode: mode, RecipeTrusted: trustRequested || alreadyTrusted || value.RecipeSource != "local", UpdatedAt: time.Now().UTC(),
				}})
			}
			database.Close()
			value := plan.Plan{ID: "bundle-configure-v1", Title: "Configure ToolTend bundle lifecycle policies"}
			for _, item := range desired {
				item := item
				value.Operations = append(value.Operations, plan.FuncOperation{
					Description: plan.OperationPreview{ID: "configure-" + item.bundle.ID, Kind: plan.OperationDatabase, Target: item.bundle.Slug,
						Summary: "Set the bundle lifecycle policy without installing or updating it", RequiresConfirmation: true,
						Details: map[string]string{"mode": string(item.policy.Mode), "owner": string(item.bundle.Owner), "recipe_source": item.bundle.RecipeSource}},
					ApplyFunc: func(ctx context.Context) error {
						return withLifecycleStateLock(ctx, paths, func(db *store.Store) error { return db.ConfigureBundle(ctx, item.policy) })
					},
				})
			}
			return a.applyPlan(ctx, value, func() any {
				result := make([]model.BundlePolicy, 0, len(desired))
				for _, item := range desired {
					result = append(result, item.policy)
				}
				return result
			})
		})(cmd, nil)
	}
	return command
}

func (a *App) newBundlesUpdateCommand() *cobra.Command {
	var all, stageOnly bool
	command := &cobra.Command{Use: "update [bundle]", Short: "Resolve and transactionally update a configured bundle", Args: cobra.MaximumNArgs(1)}
	command.Flags().BoolVar(&all, "all", false, "update all configured auto/manual bundles")
	command.Flags().BoolVar(&stageOnly, "stage-only", false, "stage every artifact without activation")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("bundles update", func(ctx context.Context) (any, error) {
			if all == (len(args) == 1) {
				return nil, cliError("invalid_argument", "provide one bundle or --all", nil)
			}
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			var targets []model.Bundle
			if all {
				values, listErr := database.ListBundles(ctx)
				if listErr != nil {
					database.Close()
					return nil, listErr
				}
				for _, value := range values {
					policy, policyErr := database.GetBundlePolicy(ctx, value.ID)
					if policyErr == nil && (policy.Mode == model.BundlePolicyAuto || policy.Mode == model.BundlePolicyManual) {
						targets = append(targets, value)
					}
				}
			} else {
				value, resolveErr := resolveBundle(ctx, database, args[0])
				if resolveErr != nil {
					database.Close()
					return nil, resolveErr
				}
				targets = append(targets, value)
			}
			database.Close()
			if len(targets) == 0 {
				return nil, cliError("not_configured", "no configured bundle can be updated", nil)
			}
			var previews []bundle.UpdatePreview
			for _, target := range targets {
				db, openErr := store.OpenRW(paths.DatabaseFile)
				if openErr != nil {
					return nil, openErr
				}
				service := bundle.Service{Database: db, Paths: paths, Runner: a.runner}
				preview, prepareErr := service.PrepareUpdate(ctx, target.ID, stageOnly)
				_ = db.Close()
				if prepareErr != nil {
					return nil, prepareErr
				}
				if preview.UpdateAvailable {
					previews = append(previews, preview)
				}
			}
			var results []bundle.UpdateResult
			value := plan.Plan{ID: "bundle-update-v1", Title: "Update complete ToolTend bundles"}
			for _, preview := range previews {
				preview := preview
				value.Operations = append(value.Operations, plan.FuncOperation{
					Description: plan.OperationPreview{ID: "update-" + preview.Bundle.ID, Kind: plan.OperationActivate, Target: preview.Bundle.Slug,
						Summary: "Stage every artifact, activate once, validate health, and compensate in reverse on failure", Reversible: false, RequiresConfirmation: true,
						Details: map[string]string{"current_release": releaseID(preview.Current), "target_release": preview.Target.Version, "stage_only": fmt.Sprint(stageOnly)}},
					ApplyFunc: func(ctx context.Context) error {
						return withLifecycleStateLock(ctx, paths, func(db *store.Store) error {
							service := bundle.Service{Database: db, Paths: paths, Runner: a.runner}
							result, executeErr := service.ExecuteUpdate(ctx, preview)
							if executeErr == nil {
								results = append(results, result)
							}
							return executeErr
						})
					},
				})
			}
			return a.applyPlan(ctx, value, func() any { return results })
		})(cmd, args)
	}
	return command
}

func (a *App) newBundlesRollbackCommand() *cobra.Command {
	var target string
	command := &cobra.Command{Use: "rollback <bundle>", Short: "Roll back a bundle to a prior release receipt", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&target, "to", "", "release or receipt ID; defaults to the previous successful receipt")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("bundles rollback", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			value, err := resolveBundle(ctx, database, args[0])
			if err != nil {
				database.Close()
				return nil, err
			}
			receipts, err := database.ListBundleReceipts(ctx, value.ID, 100)
			if err != nil {
				database.Close()
				return nil, err
			}
			selected := selectRollbackReceipt(receipts, target, value.CurrentReleaseID)
			database.Close()
			if selected.ID == "" {
				return nil, cliError("not_found", "no matching prior bundle receipt was found", nil)
			}
			writeDatabase, err := store.OpenRW(paths.DatabaseFile)
			if err != nil {
				return nil, err
			}
			service := bundle.Service{Database: writeDatabase, Paths: paths, Runner: a.runner}
			preview, err := service.PrepareRollback(ctx, value.ID, selected.ReleaseID)
			_ = writeDatabase.Close()
			if err != nil {
				return nil, err
			}
			var result bundle.RollbackResult
			valuePlan := plan.Plan{ID: "bundle-rollback-v1", Title: "Roll back a complete ToolTend bundle"}
			valuePlan.Operations = append(valuePlan.Operations, plan.FuncOperation{
				Description: plan.OperationPreview{ID: "rollback-" + value.ID, Kind: plan.OperationActivate, Target: value.Slug,
					Summary:              "Roll back every installation, validate health, and restore the current release on failure",
					RequiresConfirmation: true, Details: map[string]string{"from_release": preview.From.Version, "to_release": preview.To.Version, "steps": fmt.Sprint(preview.Steps)}},
				ApplyFunc: func(ctx context.Context) error {
					return withLifecycleStateLock(ctx, paths, func(db *store.Store) error {
						service := bundle.Service{Database: db, Paths: paths, Runner: a.runner}
						applied, executeErr := service.ExecuteRollback(ctx, preview)
						if executeErr == nil {
							result = applied
						}
						return executeErr
					})
				},
			})
			return a.applyPlan(ctx, valuePlan, func() any { return result })
		})(cmd, args)
	}
	return command
}

func (a *App) newBundlesHistoryCommand() *cobra.Command {
	var limit int
	command := &cobra.Command{Use: "history [bundle]", Short: "Show bundle-level update and rollback receipts", Args: cobra.MaximumNArgs(1)}
	command.Flags().IntVar(&limit, "limit", 100, "maximum receipts")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("bundles history", func(ctx context.Context) (any, error) {
			if limit <= 0 {
				return nil, cliError("invalid_argument", "limit must be positive", nil)
			}
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			defer database.Close()
			bundleID := ""
			if len(args) == 1 {
				value, resolveErr := resolveBundle(ctx, database, args[0])
				if resolveErr != nil {
					return nil, resolveErr
				}
				bundleID = value.ID
			}
			return database.ListBundleReceipts(ctx, bundleID, limit)
		})(cmd, args)
	}
	return command
}

type bundleDoctorReport struct {
	Healthy bool                `json:"healthy"`
	Bundle  *model.Bundle       `json:"bundle,omitempty"`
	Checks  []bundleDoctorCheck `json:"checks"`
}

type bundleDoctorCheck struct {
	Name    string `json:"name"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

func (a *App) newBundlesDoctorCommand() *cobra.Command {
	command := &cobra.Command{Use: "doctor [bundle]", Short: "Validate bundle coverage, ownership, and transaction health", Args: cobra.MaximumNArgs(1)}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("bundles doctor", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			defer database.Close()
			report := bundleDoctorReport{Healthy: true}
			if len(args) == 0 {
				counts, countErr := database.BundleCounts(ctx)
				if countErr != nil {
					return nil, countErr
				}
				level, message := "ok", fmt.Sprintf("%d bundles discovered; %d configured", counts.Total, counts.Configured)
				if counts.Configured == 0 {
					level, message = "warning", "infrastructure is healthy but bundle management coverage is zero; run tooltend bundles configure"
				}
				report.Checks = append(report.Checks, bundleDoctorCheck{Name: "coverage", Level: level, Message: message})
				if counts.FailedTransactions > 0 {
					report.Healthy = false
					report.Checks = append(report.Checks, bundleDoctorCheck{Name: "transactions", Level: "error", Message: fmt.Sprintf("%d bundle transactions failed", counts.FailedTransactions)})
				} else {
					report.Checks = append(report.Checks, bundleDoctorCheck{Name: "transactions", Level: "ok", Message: "no failed bundle transactions"})
				}
				return report, nil
			}
			value, err := resolveBundle(ctx, database, args[0])
			if err != nil {
				return nil, err
			}
			report.Bundle = &value
			installations, err := database.ListInstallations(ctx, value.ID)
			if err != nil {
				return nil, err
			}
			if len(installations) == 0 {
				report.Healthy = false
				report.Checks = append(report.Checks, bundleDoctorCheck{Name: "installations", Level: "error", Message: "bundle has no physical installations"})
			} else {
				report.Checks = append(report.Checks, bundleDoctorCheck{Name: "installations", Level: "ok", Message: fmt.Sprintf("%d physical installations", len(installations))})
			}
			if value.ConfigState == model.BundleUnconfigured {
				report.Checks = append(report.Checks, bundleDoctorCheck{Name: "policy", Level: "warning", Message: "bundle is unconfigured and will not be checked or updated"})
			} else {
				report.Checks = append(report.Checks, bundleDoctorCheck{Name: "policy", Level: "ok", Message: "bundle is configured"})
			}
			return report, nil
		})(cmd, args)
	}
	return command
}

func loadBundleSummary(ctx context.Context, database *store.Store, value model.Bundle) (bundleSummary, error) {
	result := bundleSummary{Bundle: value, Status: "unconfigured"}
	policy, err := database.GetBundlePolicy(ctx, value.ID)
	if err == nil {
		result.Policy, result.Status = &policy, string(policy.Mode)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	artifacts, err := database.ListBundleArtifacts(ctx, value.ID)
	if err != nil {
		return result, err
	}
	result.Artifacts = len(artifacts)
	installations, err := database.ListInstallations(ctx, value.ID)
	if err != nil {
		return result, err
	}
	result.Installations = len(installations)
	for _, installation := range installations {
		consumers, listErr := database.ListConsumerBindings(ctx, installation.ID)
		if listErr != nil {
			return result, listErr
		}
		result.Consumers += len(consumers)
	}
	if value.CurrentReleaseID != "" {
		release, releaseErr := database.GetBundleRelease(ctx, value.CurrentReleaseID)
		if releaseErr == nil {
			result.CurrentRelease = &release
		} else if !errors.Is(releaseErr, sql.ErrNoRows) {
			return result, releaseErr
		}
	}
	return result, nil
}

func loadBundleDetail(ctx context.Context, database *store.Store, value model.Bundle) (bundleDetail, error) {
	result := bundleDetail{Bundle: value}
	policy, err := database.GetBundlePolicy(ctx, value.ID)
	if err == nil {
		result.Policy = &policy
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if value.CurrentReleaseID != "" {
		release, releaseErr := database.GetBundleRelease(ctx, value.CurrentReleaseID)
		if releaseErr == nil {
			result.CurrentRelease = &release
		} else if !errors.Is(releaseErr, sql.ErrNoRows) {
			return result, releaseErr
		}
	}
	artifacts, err := database.ListBundleArtifacts(ctx, value.ID)
	if err != nil {
		return result, err
	}
	installations, err := database.ListInstallations(ctx, value.ID)
	if err != nil {
		return result, err
	}
	byArtifact := map[string][]model.Installation{}
	for _, installation := range installations {
		byArtifact[installation.ArtifactID] = append(byArtifact[installation.ArtifactID], installation)
	}
	for _, artifact := range artifacts {
		item := bundleArtifactDetail{Artifact: artifact}
		for _, installation := range byArtifact[artifact.ID] {
			consumers, listErr := database.ListConsumerBindings(ctx, installation.ID)
			if listErr != nil {
				return result, listErr
			}
			item.Installations = append(item.Installations, bundleInstallationDetail{Installation: installation, Consumers: consumers})
		}
		result.Artifacts = append(result.Artifacts, item)
	}
	result.History, err = database.ListBundleReceipts(ctx, value.ID, 20)
	if err != nil {
		return result, err
	}
	result.Health, err = database.ListBundleHealthChecks(ctx, value.ID, 20)
	return result, err
}

func resolveBundle(ctx context.Context, database *store.Store, selector string) (model.Bundle, error) {
	if value, err := database.GetBundle(ctx, selector); err == nil {
		return value, nil
	}
	if value, err := database.GetBundleBySlug(ctx, selector); err == nil {
		return value, nil
	}
	values, err := database.ListBundles(ctx)
	if err != nil {
		return model.Bundle{}, err
	}
	var matches []model.Bundle
	for _, value := range values {
		if strings.EqualFold(value.Name, selector) {
			matches = append(matches, value)
		}
	}
	if len(matches) == 0 {
		return model.Bundle{}, sql.ErrNoRows
	}
	if len(matches) > 1 {
		return model.Bundle{}, cliError("ambiguous_selector", fmt.Sprintf("bundle selector %q is ambiguous", selector), nil)
	}
	return matches[0], nil
}

func validateBundleMode(ctx context.Context, database *store.Store, value model.Bundle, mode model.BundlePolicyMode) error {
	if mode == model.BundlePolicyIgnore {
		return nil
	}
	switch value.Owner {
	case model.LifecycleToolTend, model.LifecycleDelegated:
	case model.LifecycleHostOwned, model.LifecycleAppOwned, model.LifecycleWorkspaceLinked, model.LifecycleUnresolved:
		if mode != model.BundlePolicyObserve {
			return fmt.Errorf("bundle owner %s only supports observe or ignore", value.Owner)
		}
	}
	if mode == model.BundlePolicyAuto {
		eligible, err := bundleAutoEligible(ctx, database, value.ID)
		if err != nil {
			return err
		}
		if !eligible {
			return errors.New("bundle recipe is not eligible for auto: every installed artifact needs resolve, stage, activate, rollback, and health commands")
		}
	}
	if mode == model.BundlePolicyManual {
		checkable, err := bundleCheckable(ctx, database, value.ID)
		if err != nil {
			return err
		}
		if !checkable {
			return errors.New("bundle recipe cannot perform a manual update: installed artifacts need resolve, stage, activate, and health commands")
		}
	}
	return nil
}

func (a *App) interactiveBundleSettings(ctx context.Context, database *store.Store, values []model.Bundle) ([]string, error) {
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	scanner := bufio.NewScanner(a.in)
	result := []string{}
	for _, value := range values {
		if value.RecipeSource == "fallback" && value.Owner == model.LifecycleHostOwned {
			continue
		}
		recommended := "observe"
		if value.Owner == model.LifecycleToolTend || value.Owner == model.LifecycleDelegated {
			if eligible, _ := bundleAutoEligible(ctx, database, value.ID); eligible {
				recommended = "auto"
			} else if checkable, _ := bundleCheckable(ctx, database, value.ID); checkable {
				recommended = "manual"
			}
		}
		_, _ = fmt.Fprintf(a.out, "%s (%s) [auto/manual/observe/ignore, Enter to skip; recommended %s]: ", value.Name, value.Owner, recommended)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, err
			}
			return result, nil
		}
		answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if answer == "" {
			continue
		}
		result = append(result, value.Slug+"="+answer)
	}
	return result, nil
}

func bundleAutoEligible(ctx context.Context, database *store.Store, bundleID string) (bool, error) {
	artifacts, err := database.ListBundleArtifacts(ctx, bundleID)
	if err != nil {
		return false, err
	}
	installations, err := database.ListInstallations(ctx, bundleID)
	if err != nil {
		return false, err
	}
	installed := make(map[string]bool)
	for _, installation := range installations {
		installed[installation.ArtifactID] = true
	}
	if len(installed) == 0 {
		return false, nil
	}
	for _, artifact := range artifacts {
		if !installed[artifact.ID] {
			continue
		}
		var recipe bundle.ArtifactRecipe
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &recipe); err != nil {
			return false, err
		}
		if len(recipe.ResolveArgv) == 0 || len(recipe.StageArgv) == 0 || len(recipe.ActivateArgv) == 0 || len(recipe.RollbackArgv) == 0 || len(recipe.HealthArgv) == 0 {
			return false, nil
		}
	}
	return true, nil
}

func bundleCheckable(ctx context.Context, database *store.Store, bundleID string) (bool, error) {
	artifacts, err := database.ListBundleArtifacts(ctx, bundleID)
	if err != nil {
		return false, err
	}
	installations, err := database.ListInstallations(ctx, bundleID)
	if err != nil {
		return false, err
	}
	installed := make(map[string]bool)
	for _, installation := range installations {
		installed[installation.ArtifactID] = true
	}
	if len(installed) == 0 {
		return false, nil
	}
	for _, artifact := range artifacts {
		if !installed[artifact.ID] {
			continue
		}
		var recipe bundle.ArtifactRecipe
		if err := json.Unmarshal([]byte(artifact.MetadataJSON), &recipe); err != nil {
			return false, err
		}
		if len(recipe.ResolveArgv) == 0 || len(recipe.StageArgv) == 0 || len(recipe.ActivateArgv) == 0 || len(recipe.HealthArgv) == 0 {
			return false, nil
		}
	}
	return true, nil
}

func releaseID(value *model.BundleRelease) string {
	if value == nil {
		return ""
	}
	return value.Version
}

func selectRollbackReceipt(values []model.BundleReceipt, selector, currentReleaseID string) model.BundleReceipt {
	if selector != "" {
		for _, value := range values {
			if (value.ID == selector || value.ReleaseID == selector) && value.ReleaseID != currentReleaseID && value.Status == "succeeded" {
				return value
			}
		}
		return model.BundleReceipt{}
	}
	for _, value := range values {
		if value.Status == "succeeded" && value.ReleaseID != "" && value.ReleaseID != currentReleaseID {
			return value
		}
	}
	return model.BundleReceipt{}
}

func encodeJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
