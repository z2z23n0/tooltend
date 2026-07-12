package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type statusData struct {
	Initialized       bool     `json:"initialized"`
	Issues            []string `json:"issues,omitempty"`
	Components        int      `json:"components"`
	Bindings          int      `json:"bindings"`
	ManagedBindings   int      `json:"managed_bindings"`
	UpdatesAvailable  int      `json:"updates_available"`
	NeedsReview       int      `json:"needs_review"`
	FailedCandidates  int      `json:"failed_candidates"`
	PendingTasks      int      `json:"pending_tasks"`
	UnfinishedActions int      `json:"unfinished_activations"`
}

type componentSummary struct {
	Component         model.LogicalComponent `json:"component"`
	Source            model.Source           `json:"source"`
	Bindings          int                    `json:"bindings"`
	Managed           int                    `json:"managed_bindings"`
	HostOwnedBindings int                    `json:"host_owned_bindings,omitempty"`
	Status            string                 `json:"status"`
}

type bindingDetail struct {
	Binding    model.Binding           `json:"binding"`
	Policy     model.Policy            `json:"policy"`
	Candidates []model.UpdateCandidate `json:"candidates"`
}

type componentDetail struct {
	Component model.LogicalComponent `json:"component"`
	Source    model.Source           `json:"source"`
	Bindings  []bindingDetail        `json:"bindings"`
}

func (a *App) newStatusCommand() *cobra.Command {
	command := &cobra.Command{Use: "status", Short: "Show update and health status", Args: cobra.NoArgs}
	command.RunE = a.run("status", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		cfg, err := config.Load(paths.ConfigFile)
		if errors.Is(err, os.ErrNotExist) {
			return statusData{Initialized: false, Issues: []string{"configuration_missing"}}, nil
		} else if err != nil {
			return nil, err
		}
		database, err := a.openReadOnly(paths)
		if errors.Is(err, os.ErrNotExist) {
			return statusData{Initialized: false, Issues: []string{"database_missing"}}, nil
		}
		if err != nil {
			return nil, err
		}
		defer database.Close()
		if len(cfg.Projects) == 0 {
			return statusData{Initialized: false, Issues: []string{"project_selection_missing"}}, nil
		}
		projects, err := database.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		selectedProjectPersisted := false
		for _, project := range projects {
			if project.Selected && containsPath(cfg.Projects, project.RootPath) {
				selectedProjectPersisted = true
				break
			}
		}
		if !selectedProjectPersisted {
			return statusData{Initialized: false, Issues: []string{"project_inventory_missing"}}, nil
		}
		result := statusData{Initialized: true}
		components, err := database.ListComponents(ctx)
		if err != nil {
			return nil, err
		}
		bindings, err := database.ListBindings(ctx, "")
		if err != nil {
			return nil, err
		}
		result.Components, result.Bindings = len(components), len(bindings)
		for _, binding := range bindings {
			if binding.Managed {
				result.ManagedBindings++
			}
		}
		for _, binding := range bindings {
			if binding.HostOwned() {
				continue
			}
			policy, policyErr := database.GetPolicy(ctx, binding.ID)
			if policyErr != nil {
				return nil, policyErr
			}
			if policy.ApplyMode == model.ApplyIgnore || policy.LocalCapMode == model.ApplyIgnore {
				continue
			}
			candidates, listErr := database.ListCandidates(ctx, binding.ID, "")
			if listErr != nil {
				return nil, listErr
			}
			for _, candidate := range candidates {
				switch candidate.Status {
				case model.CandidateAvailable:
					result.UpdatesAvailable++
				case model.CandidateNeedsReview:
					result.NeedsReview++
				case model.CandidateFailed:
					result.FailedCandidates++
				}
			}
		}
		tasks, err := database.CountTasks(ctx)
		if err != nil {
			return nil, err
		}
		result.PendingTasks = tasks.Pending + tasks.Running
		intents, err := database.ListUnfinishedActivations(ctx)
		if err != nil {
			return nil, err
		}
		adoptions, err := database.ListPendingAdoptions(ctx)
		if err != nil {
			return nil, err
		}
		result.UnfinishedActions = len(intents) + len(adoptions)
		for _, intent := range adoptions {
			if intent.Phase == store.AdoptionBlocked {
				result.Issues = append(result.Issues, "adoption_recovery_blocked")
				break
			}
		}
		return result, nil
	})
	return command
}

func (a *App) newComponentsCommand() *cobra.Command {
	command := &cobra.Command{Use: "components", Short: "Inspect discovered components", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	var managedOnly, all bool
	list := &cobra.Command{Use: "list", Short: "List actionable components", Args: cobra.NoArgs}
	list.Flags().BoolVar(&managedOnly, "managed", false, "list only ToolTend-managed components")
	list.Flags().BoolVar(&all, "all", false, "include dependency-only and host-owned observations")
	list.RunE = a.run("components list", func(ctx context.Context) (any, error) {
		if managedOnly && all {
			return nil, cliError("invalid_argument", "--managed cannot be combined with --all", nil)
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
		mode := componentListActionable
		if managedOnly {
			mode = componentListManaged
		} else if all {
			mode = componentListAll
		}
		return listComponentSummaries(ctx, database, mode)
	})
	show := &cobra.Command{Use: "show <component>", Short: "Show a component and all bindings", Args: cobra.ExactArgs(1)}
	show.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("components show", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			defer database.Close()
			component, err := resolveComponent(ctx, database, args[0])
			if err != nil {
				return nil, err
			}
			return loadComponentDetail(ctx, database, component)
		})(cmd, args)
	}
	command.AddCommand(list, show)
	return command
}

type componentListMode int

const (
	componentListActionable componentListMode = iota
	componentListManaged
	componentListAll
)

func listComponentSummaries(ctx context.Context, database *store.Store, mode componentListMode) ([]componentSummary, error) {
	components, err := database.ListComponents(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]componentSummary, 0, len(components))
	for _, component := range components {
		source, err := database.GetSource(ctx, component.SourceID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		bindings, err := database.ListBindings(ctx, component.ID)
		if err != nil {
			return nil, err
		}
		visible := bindings[:0]
		for _, binding := range bindings {
			switch mode {
			case componentListManaged:
				if binding.Managed {
					visible = append(visible, binding)
				}
			case componentListAll:
				visible = append(visible, binding)
			default:
				if !binding.HostOwned() {
					visible = append(visible, binding)
				}
			}
		}
		if mode != componentListAll && len(visible) == 0 {
			continue
		}
		item := componentSummary{Component: component, Source: source, Bindings: len(visible), Status: "current"}
		for _, binding := range visible {
			if binding.Managed {
				item.Managed++
			}
			if binding.HostOwned() {
				item.HostOwnedBindings++
			}
			candidates, listErr := database.ListCandidates(ctx, binding.ID, "")
			if listErr != nil {
				return nil, listErr
			}
			for _, candidate := range candidates {
				switch candidate.Status {
				case model.CandidateNeedsReview:
					item.Status = "needs_review"
				case model.CandidateAvailable:
					if item.Status == "current" {
						item.Status = "available"
					}
				case model.CandidateFailed:
					if item.Status == "current" {
						item.Status = "failed"
					}
				}
			}
		}
		result = append(result, item)
	}
	return result, nil
}

func resolveComponent(ctx context.Context, database *store.Store, selector string) (model.LogicalComponent, error) {
	components, err := database.ListComponents(ctx)
	if err != nil {
		return model.LogicalComponent{}, err
	}
	var matches []model.LogicalComponent
	for _, component := range components {
		if component.ID == selector || component.LogicalKey == selector || strings.EqualFold(component.Name, selector) {
			matches = append(matches, component)
		}
	}
	if len(matches) == 0 {
		return model.LogicalComponent{}, sql.ErrNoRows
	}
	if len(matches) > 1 {
		return model.LogicalComponent{}, cliError("ambiguous_selector", fmt.Sprintf("component selector %q is ambiguous", selector), nil)
	}
	return matches[0], nil
}

func loadComponentDetail(ctx context.Context, database *store.Store, component model.LogicalComponent) (componentDetail, error) {
	result := componentDetail{Component: component}
	if component.SourceID != "" {
		source, err := database.GetSource(ctx, component.SourceID)
		if err != nil {
			return result, err
		}
		result.Source = source
	}
	bindings, err := database.ListBindings(ctx, component.ID)
	if err != nil {
		return result, err
	}
	for _, binding := range bindings {
		item := bindingDetail{Binding: binding}
		item.Policy, err = database.GetPolicy(ctx, binding.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		item.Candidates, err = database.ListCandidates(ctx, binding.ID, "")
		if err != nil {
			return result, err
		}
		result.Bindings = append(result.Bindings, item)
	}
	return result, nil
}

func (a *App) newHistoryCommand() *cobra.Command {
	var limit int
	command := &cobra.Command{Use: "history [component]", Short: "Show update, adopt, and rollback receipts", Args: cobra.MaximumNArgs(1)}
	command.Flags().IntVar(&limit, "limit", 100, "maximum receipts to return")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("history", func(ctx context.Context) (any, error) {
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
			if len(args) == 0 {
				return database.ListReceipts(ctx, "", limit)
			}
			component, err := resolveComponent(ctx, database, args[0])
			if err != nil {
				return nil, err
			}
			bindings, err := database.ListBindings(ctx, component.ID)
			if err != nil {
				return nil, err
			}
			var receipts []model.Receipt
			for _, binding := range bindings {
				items, listErr := database.ListReceipts(ctx, binding.ID, limit)
				if listErr != nil {
					return nil, listErr
				}
				receipts = append(receipts, items...)
			}
			sort.Slice(receipts, func(i, j int) bool { return receipts[i].CreatedAt.After(receipts[j].CreatedAt) })
			if len(receipts) > limit {
				receipts = receipts[:limit]
			}
			return receipts, nil
		})(cmd, args)
	}
	return command
}
