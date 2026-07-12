package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/lifecycle"
	"github.com/z2z23n0/tooltend/internal/lockfile"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/store"
)

type lifecycleAPI interface {
	ResolveUpdate(context.Context, string, string) (lifecycle.UpdateTarget, error)
	Update(context.Context, string, string, lifecycle.UpdateOptions) (lifecycle.UpdateResult, error)
	ResolveAdopt(context.Context, string, lifecycle.AdoptOptions) (lifecycle.AdoptTarget, error)
	Adopt(context.Context, string, lifecycle.AdoptOptions) (lifecycle.AdoptResult, error)
	Rollback(context.Context, string, string, string) (lifecycle.RollbackResult, error)
	ResolveRollbackTarget(context.Context, string, string, string) (lifecycle.RollbackTarget, error)
	RollbackWithOptions(context.Context, string, string, lifecycle.RollbackOptions) (lifecycle.RollbackResult, error)
}

func (a *App) withLifecycle(paths config.Paths, action func(*store.Store, lifecycleAPI) error) error {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return err
	}
	if cfg.Runtime.ShimDir != "" {
		if !filepath.IsAbs(cfg.Runtime.ShimDir) {
			return cliError("invalid_configuration", "runtime shim directory must be absolute", nil)
		}
		paths.ShimDir = cfg.Runtime.ShimDir
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		return err
	}
	defer database.Close()
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		return err
	}
	service, err := lifecycle.New(database, objects, paths)
	if err != nil {
		return err
	}
	return action(database, service)
}

func withLifecycleStateLock(ctx context.Context, paths config.Paths, action func(*store.Store) error) (err error) {
	if cfg, loadErr := config.Load(paths.ConfigFile); loadErr == nil && cfg.Runtime.ShimDir != "" {
		if !filepath.IsAbs(cfg.Runtime.ShimDir) {
			return errors.New("runtime shim directory must be absolute")
		}
		paths.ShimDir = filepath.Clean(cfg.Runtime.ShimDir)
	} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	lock, err := lockfile.Try(paths.ActivationLock)
	if err != nil {
		return fmt.Errorf("acquire activation lock: %w", err)
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
	if _, err := lifecycle.RecoverActivations(ctx, database, paths); err != nil {
		return err
	}
	return action(database)
}

func (a *App) newPolicyCommand() *cobra.Command {
	parent := &cobra.Command{Use: "policy", Short: "Manage binding update policy", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var bindingID, mode, track, constraint, notify string
	set := &cobra.Command{Use: "set <component>", Short: "Set track, apply, or notification policy", Args: cobra.ExactArgs(1)}
	set.Flags().StringVar(&bindingID, "binding", "", "limit the change to one binding")
	set.Flags().StringVar(&mode, "mode", "", "apply mode: auto, manual, or ignore")
	set.Flags().StringVar(&track, "track", "", "track: stable, latest, main, semver, or exact")
	set.Flags().StringVar(&constraint, "constraint", "", "semver constraint or exact version")
	set.Flags().StringVar(&notify, "notify", "", "notification mode: all, failures, or none")
	set.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("policy set", func(ctx context.Context) (any, error) {
			if mode == "" && track == "" && notify == "" && !cmd.Flags().Changed("constraint") {
				return nil, cliError("invalid_argument", "set at least one of --mode, --track, --constraint, or --notify", nil)
			}
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			component, err := resolveComponent(ctx, database, args[0])
			if err != nil {
				database.Close()
				return nil, err
			}
			bindings, err := database.ListBindings(ctx, component.ID)
			if err != nil {
				database.Close()
				return nil, err
			}
			var desired []model.Policy
			before := make(map[string]model.Policy)
			for _, binding := range bindings {
				if bindingID != "" && binding.ID != bindingID {
					continue
				}
				if binding.HostOwned() && mode != "" && model.ApplyMode(mode) != model.ApplyIgnore {
					database.Close()
					return nil, cliError("observe_only", fmt.Sprintf("binding lifecycle is owned by %s", binding.LifecycleOwner()), nil)
				}
				policy, getErr := database.GetPolicy(ctx, binding.ID)
				if getErr != nil {
					database.Close()
					return nil, getErr
				}
				before[binding.ID] = policy
				if mode != "" {
					policy.ApplyMode = model.ApplyMode(mode)
				}
				if track != "" {
					policy.TrackChannel = model.TrackChannel(track)
				}
				if cmd.Flags().Changed("constraint") {
					policy.Constraint = constraint
				}
				if track != "" || cmd.Flags().Changed("constraint") {
					policy.ExpectedIntegrity = ""
				}
				if notify != "" {
					policy.NotifyMode = model.NotifyMode(notify)
				}
				policy.UpdatedAt = time.Now().UTC()
				if validateErr := policy.Validate(); validateErr != nil {
					database.Close()
					return nil, cliError("invalid_argument", validateErr.Error(), validateErr)
				}
				if policy.TrackChannel == model.TrackExact && strings.TrimSpace(policy.Constraint) == "" {
					database.Close()
					return nil, cliError("invalid_argument", "exact track requires --constraint", nil)
				}
				desired = append(desired, policy)
			}
			database.Close()
			if len(desired) == 0 {
				return nil, sql.ErrNoRows
			}
			value := plan.Plan{ID: "policy-set-v1", Title: "Update ToolTend binding policy"}
			for _, policy := range desired {
				policy := policy
				expected := before[policy.BindingID]
				value.Operations = append(value.Operations, plan.FuncOperation{
					Description: plan.OperationPreview{
						ID: "set-policy-" + policy.BindingID, Kind: plan.OperationDatabase, Target: policy.BindingID,
						Summary:              "Set local track, apply, and notification policy for the binding",
						RequiresConfirmation: true,
						Details: map[string]string{
							"track": string(policy.TrackChannel), "constraint": policy.Constraint,
							"mode": string(policy.ApplyMode), "notify": string(policy.NotifyMode),
							"integrity_cleared": fmt.Sprint(expected.ExpectedIntegrity != "" && policy.ExpectedIntegrity == ""),
						},
					},
					ApplyFunc: func(ctx context.Context) error {
						return withLifecycleStateLock(ctx, paths, func(db *store.Store) error {
							return db.CompareAndSetPolicy(ctx, expected, policy)
						})
					},
				})
			}
			return a.applyPlan(ctx, value, func() any { return desired })
		})(cmd, args)
	}
	parent.AddCommand(set)
	return parent
}

type updateTarget struct {
	ComponentID       string `json:"component_id"`
	Name              string `json:"name"`
	BindingID         string `json:"binding_id"`
	Source            string `json:"source"`
	CurrentRef        string `json:"current_ref,omitempty"`
	CurrentGeneration string `json:"current_generation,omitempty"`
	TargetRef         string `json:"target_ref,omitempty"`
}

func (a *App) newUpdateCommand() *cobra.Command {
	var all, stageOnly bool
	var bindingID string
	command := &cobra.Command{Use: "update [component]", Short: "Check, stage, validate, and apply an update", Args: cobra.MaximumNArgs(1)}
	command.Flags().BoolVar(&all, "all", false, "update all non-ignored bindings")
	command.Flags().BoolVar(&stageOnly, "stage-only", false, "stage and validate without activation")
	command.Flags().StringVar(&bindingID, "binding", "", "update one binding")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("update", func(ctx context.Context) (any, error) {
			if all == (len(args) == 1) {
				return nil, cliError("invalid_argument", "provide one component or --all", nil)
			}
			if all && bindingID != "" {
				return nil, cliError("invalid_argument", "--binding cannot be combined with --all", nil)
			}
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			targets, err := a.updateTargets(ctx, paths, args, all, bindingID)
			if err != nil {
				return nil, err
			}
			var results []lifecycle.UpdateResult
			value := plan.Plan{ID: "update-v1", Title: "Update ToolTend-managed component bindings"}
			for _, target := range targets {
				target := target
				value.Operations = append(value.Operations, plan.FuncOperation{
					Description: plan.OperationPreview{
						ID: "update-" + target.BindingID, Kind: plan.OperationActivate, Target: target.BindingID,
						Summary:              "Resolve, stage, validate, and atomically activate an update with rollback protection",
						RequiresConfirmation: true,
						Details: map[string]string{
							"component": target.Name, "source": target.Source,
							"current_ref": target.CurrentRef, "current_generation": target.CurrentGeneration,
							"target_ref": target.TargetRef,
							"stage_only": fmt.Sprint(stageOnly),
						},
					},
					ApplyFunc: func(ctx context.Context) error {
						return a.withLifecycle(paths, func(_ *store.Store, service lifecycleAPI) error {
							result, updateErr := service.Update(ctx, target.ComponentID, target.BindingID, lifecycle.UpdateOptions{
								Stage: true, Activate: !stageOnly, Reason: "manual_cli",
								ExpectedRef: target.TargetRef, ExpectedGeneration: target.CurrentGeneration, BindGeneration: true,
							})
							if updateErr == nil {
								results = append(results, result)
							}
							return updateErr
						})
					},
				})
			}
			return a.applyPlan(ctx, value, func() any { return results })
		})(cmd, args)
	}
	return command
}

func (a *App) updateTargets(ctx context.Context, paths config.Paths, args []string, all bool, bindingID string) ([]updateTarget, error) {
	database, err := a.openReadOnly(paths)
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
	components, err := database.ListComponents(ctx)
	if err != nil {
		return nil, err
	}
	if !all {
		component, resolveErr := resolveComponent(ctx, database, args[0])
		if resolveErr != nil {
			return nil, resolveErr
		}
		if component.Kind == model.ComponentHTTPMCP {
			return nil, cliError("observe_only", "remote HTTP MCP components are observed for configuration and availability but do not have ToolTend-managed versions", nil)
		}
		components = []model.LogicalComponent{component}
	}
	var targets []updateTarget
	for _, component := range components {
		if component.Kind == model.ComponentHTTPMCP {
			continue
		}
		source, sourceErr := database.GetSource(ctx, component.SourceID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		if source.Kind == model.SourceHTTP || source.Kind == model.SourceLocal || source.Kind == model.SourceUnknown {
			if !all {
				return nil, cliError("observe_only", "this source is observed but cannot be resolved or updated safely", nil)
			}
			continue
		}
		sourceDisplay := source.PackageName
		if sourceDisplay != "" {
			sourceDisplay = string(source.Kind) + ":" + sourceDisplay
		} else {
			sourceDisplay = safeSourceForPreview(source.Locator)
		}
		bindings, listErr := database.ListBindings(ctx, component.ID)
		if listErr != nil {
			return nil, listErr
		}
		for _, binding := range bindings {
			if bindingID != "" && binding.ID != bindingID {
				continue
			}
			policy, policyErr := database.GetPolicy(ctx, binding.ID)
			if policyErr != nil {
				return nil, policyErr
			}
			if policy.ApplyMode == model.ApplyIgnore {
				continue
			}
			resolved, resolveErr := service.ResolveUpdate(ctx, component.ID, binding.ID)
			if resolveErr != nil {
				return nil, resolveErr
			}
			currentRef := binding.ObservedVersion
			if binding.ActiveGenerationID != "" {
				if generation, generationErr := database.GetGeneration(ctx, binding.ID, binding.ActiveGenerationID); generationErr == nil {
					currentRef = generation.ResolvedRef
				}
			}
			targets = append(targets, updateTarget{
				ComponentID: component.ID, Name: component.Name, BindingID: binding.ID,
				Source: sourceDisplay, CurrentRef: currentRef, CurrentGeneration: resolved.ActiveGeneration,
				TargetRef: resolved.ResolvedRef,
			})
		}
	}
	if len(targets) == 0 {
		return nil, cliError("not_found", "no eligible binding matched the update request", nil)
	}
	return targets, nil
}

func (a *App) newAdoptCommand() *cobra.Command {
	var source, subdir, version, executable, bindingID string
	command := &cobra.Command{Use: "adopt <component>", Short: "Move a component behind a managed generation or stable shim", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&source, "source", "", "canonical git, npm, pypi, brew, local, or HTTP source")
	command.Flags().StringVar(&subdir, "subdir", "", "relative component directory inside a Git repository")
	command.Flags().StringVar(&version, "version", "", "exact version or commit to verify")
	command.Flags().StringVar(&executable, "executable", "", "runtime executable exposed by the stable shim")
	command.Flags().StringVar(&bindingID, "binding", "", "adopt one binding")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("adopt", func(ctx context.Context) (any, error) {
			if strings.TrimSpace(source) == "" {
				return nil, cliError("invalid_argument", "--source is required", nil)
			}
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			componentID, selectedBinding, err := a.resolveLifecycleTarget(ctx, paths, args[0], bindingID)
			if err != nil {
				return nil, err
			}
			adoptOptions := lifecycle.AdoptOptions{Source: source, Subdir: subdir, Version: version, Executable: executable, BindingID: selectedBinding}
			adoptTarget, err := a.resolveAdoptTarget(ctx, paths, componentID, adoptOptions)
			if err != nil {
				return nil, err
			}
			var result lifecycle.AdoptResult
			value := plan.Plan{ID: "adopt-v1", Title: "Adopt a component into ToolTend management", Operations: []plan.Operation{
				plan.FuncOperation{
					Description: plan.OperationPreview{
						ID: "adopt-component", Kind: plan.OperationActivate, Target: selectedBinding,
						Summary:              "Capture the current installation, build a managed generation, and switch through an atomic pointer or shim",
						RequiresConfirmation: true,
						Details: map[string]string{
							"source": safeSourceForPreview(source), "requested_version": version,
							"resolved_ref": adoptTarget.ResolvedRef, "resolved_version": adoptTarget.Version,
							"binding": selectedBinding, "subdir": adoptTarget.Subdir, "executable": executable,
							"source_identity": adoptTarget.SourceIdentity,
							"observed_hash":   adoptTarget.ObservedHash, "config_hash": adoptTarget.ConfigHash,
						},
					},
					ApplyFunc: func(ctx context.Context) error {
						return a.withLifecycle(paths, func(_ *store.Store, service lifecycleAPI) error {
							var adoptErr error
							adoptOptions.ExpectedSourceIdentity = adoptTarget.SourceIdentity
							adoptOptions.ExpectedResolvedRef = adoptTarget.ResolvedRef
							adoptOptions.ExpectedObservedHash = adoptTarget.ObservedHash
							adoptOptions.ExpectedConfigHash = adoptTarget.ConfigHash
							result, adoptErr = service.Adopt(ctx, componentID, adoptOptions)
							return adoptErr
						})
					},
				},
			}}
			return a.applyPlan(ctx, value, func() any { return result })
		})(cmd, args)
	}
	return command
}

func (a *App) resolveAdoptTarget(ctx context.Context, paths config.Paths, componentID string, options lifecycle.AdoptOptions) (lifecycle.AdoptTarget, error) {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return lifecycle.AdoptTarget{}, err
	}
	if cfg.Runtime.ShimDir != "" {
		if !filepath.IsAbs(cfg.Runtime.ShimDir) {
			return lifecycle.AdoptTarget{}, cliError("invalid_configuration", "runtime shim directory must be absolute", nil)
		}
		paths.ShimDir = cfg.Runtime.ShimDir
	}
	database, err := a.openReadOnly(paths)
	if err != nil {
		return lifecycle.AdoptTarget{}, err
	}
	defer database.Close()
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		return lifecycle.AdoptTarget{}, err
	}
	service, err := lifecycle.New(database, objects, paths)
	if err != nil {
		return lifecycle.AdoptTarget{}, err
	}
	return service.ResolveAdopt(ctx, componentID, options)
}

func (a *App) newRollbackCommand() *cobra.Command {
	var target, bindingID string
	command := &cobra.Command{Use: "rollback <component>", Short: "Atomically return to a verified older generation", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&target, "to", "", "receipt ID, generation, or resolved version")
	command.Flags().StringVar(&bindingID, "binding", "", "rollback one binding")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("rollback", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			componentID, selectedBinding, err := a.resolveLifecycleTarget(ctx, paths, args[0], bindingID)
			if err != nil {
				return nil, err
			}
			rollbackTarget, err := a.resolveRollbackTarget(ctx, paths, componentID, selectedBinding, target)
			if err != nil {
				return nil, err
			}
			var result lifecycle.RollbackResult
			value := plan.Plan{ID: "rollback-v1", Title: "Rollback a ToolTend-managed binding", Operations: []plan.Operation{
				plan.FuncOperation{
					Description: plan.OperationPreview{
						ID: "rollback-component", Kind: plan.OperationActivate, Target: selectedBinding,
						Summary:              "Verify the target generation and atomically switch the active pointer while preserving the newer generation",
						RequiresConfirmation: true,
						Details: map[string]string{
							"binding":         selectedBinding,
							"from_generation": rollbackTarget.FromGeneration, "from_ref": rollbackTarget.FromRef,
							"to_generation": rollbackTarget.ToGeneration, "to_ref": rollbackTarget.ToRef,
						},
					},
					ApplyFunc: func(ctx context.Context) error {
						return a.withLifecycle(paths, func(_ *store.Store, service lifecycleAPI) error {
							var rollbackErr error
							result, rollbackErr = service.RollbackWithOptions(ctx, componentID, selectedBinding, lifecycle.RollbackOptions{
								To: rollbackTarget.ToGeneration, ExpectedFromGeneration: rollbackTarget.FromGeneration,
							})
							return rollbackErr
						})
					},
				},
			}}
			return a.applyPlan(ctx, value, func() any { return result })
		})(cmd, args)
	}
	return command
}

func (a *App) resolveRollbackTarget(ctx context.Context, paths config.Paths, componentID, bindingID, requested string) (lifecycle.RollbackTarget, error) {
	cfg, err := config.Load(paths.ConfigFile)
	if err != nil {
		return lifecycle.RollbackTarget{}, err
	}
	if cfg.Runtime.ShimDir != "" {
		paths.ShimDir = cfg.Runtime.ShimDir
	}
	database, err := a.openReadOnly(paths)
	if err != nil {
		return lifecycle.RollbackTarget{}, err
	}
	defer database.Close()
	objects, err := objectstore.New(paths.ObjectsDir)
	if err != nil {
		return lifecycle.RollbackTarget{}, err
	}
	service, err := lifecycle.New(database, objects, paths)
	if err != nil {
		return lifecycle.RollbackTarget{}, err
	}
	return service.ResolveRollbackTarget(ctx, componentID, bindingID, requested)
}

func (a *App) resolveLifecycleTarget(ctx context.Context, paths config.Paths, selector, bindingID string) (string, string, error) {
	database, err := a.openReadOnly(paths)
	if err != nil {
		return "", "", err
	}
	defer database.Close()
	component, err := resolveComponent(ctx, database, selector)
	if err != nil {
		return "", "", err
	}
	bindings, err := database.ListBindings(ctx, component.ID)
	if err != nil {
		return "", "", err
	}
	if bindingID != "" {
		for _, binding := range bindings {
			if binding.ID == bindingID {
				return component.ID, binding.ID, nil
			}
		}
		return "", "", sql.ErrNoRows
	}
	if len(bindings) == 0 {
		return "", "", sql.ErrNoRows
	}
	if len(bindings) > 1 {
		return "", "", cliError("ambiguous_binding", "component has multiple bindings; select one with --binding", nil)
	}
	return component.ID, bindings[0].ID, nil
}

func safeSourceForPreview(value string) string {
	prefix, raw := "", strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(raw), "git@") {
		if colon := strings.IndexByte(raw, ':'); colon > 0 {
			authority := raw[:colon]
			if at := strings.LastIndexByte(authority, '@'); at >= 0 && at+1 < len(authority) {
				return "git@" + authority[at+1:] + raw[colon:]
			}
		}
	}
	lower := strings.ToLower(raw)
	for _, kind := range []string{"git", "http", "https"} {
		marker := kind + ":"
		if strings.HasPrefix(lower, marker) {
			candidate := strings.TrimSpace(raw[len(marker):])
			if strings.Contains(candidate, "://") {
				prefix, raw = marker, candidate
			}
			break
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value)
	}
	parsed.User, parsed.RawQuery, parsed.Fragment = nil, "", ""
	return prefix + parsed.String()
}
