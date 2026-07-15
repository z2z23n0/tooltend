package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/buildinfo"
	"github.com/z2z23n0/tooltend/internal/bundle"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/host"
	"github.com/z2z23n0/tooltend/internal/inventory"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/safeio"
	"github.com/z2z23n0/tooltend/internal/scheduler"
	"github.com/z2z23n0/tooltend/internal/store"
	"golang.org/x/sys/unix"
)

type initOptions struct {
	Projects   []string
	Agents     []string
	ResetState bool
}

type profileMutation struct {
	Path       string
	Content    []byte
	Mode       os.FileMode
	BeforeHash string
	AfterHash  string
}

type initInventoryItem struct {
	Host            host.Name            `json:"host"`
	Kind            host.ComponentKind   `json:"kind"`
	Name            string               `json:"name"`
	Version         string               `json:"version,omitempty"`
	InstallPath     string               `json:"install_path,omitempty"`
	Project         string               `json:"project,omitempty"`
	Source          host.SourceRef       `json:"source"`
	Classification  model.Classification `json:"classification"`
	RecommendedMode model.ApplyMode      `json:"recommended_mode"`
	DuplicateCopies int                  `json:"duplicate_copies"`
	VersionDrift    bool                 `json:"version_drift"`
}

func (a *App) newInitCommand() *cobra.Command {
	var options initOptions
	command := &cobra.Command{
		Use:   "init",
		Short: "Discover extensions and initialize ToolTend",
		Args:  cobra.NoArgs,
	}
	command.Flags().StringSliceVar(&options.Projects, "project", nil, "select a project (repeatable)")
	command.Flags().StringSliceVar(&options.Agents, "agent", nil, "select codex and/or claude")
	command.Flags().BoolVar(&options.ResetState, "reset-state", false, "back up and rebuild ToolTend state without configuring bundles")
	command.RunE = a.run("init", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		if options.ResetState {
			return a.resetState(ctx, paths, options)
		}
		cfg, err := a.loadConfig(paths)
		if err != nil {
			return nil, err
		}
		if len(options.Agents) > 0 {
			cfg.Agents, err = parseAgents(options.Agents)
			if err != nil {
				return nil, err
			}
		}
		projectCandidates := inventory.DiscoverProjects(a.home, a.workingDir, 20)
		projects, err := a.initProjects(cfg.Projects, options.Projects)
		if err != nil {
			return nil, err
		}
		cfg.Projects = projects
		var profile *profileMutation
		if cfg.Runtime.ShimDir == "" {
			cfg.Runtime.ShimDir, profile, err = a.planShimPath(paths)
			if err != nil {
				return nil, err
			}
		}
		if !filepath.IsAbs(cfg.Runtime.ShimDir) {
			return nil, cliError("invalid_configuration", "runtime shim directory must be absolute", nil)
		}
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		paths.ShimDir = cfg.Runtime.ShimDir

		// Everything above and through plan construction is read-only. In
		// particular, Scan and PlanHookInstall never create their target paths.
		currentProject := ""
		if containsPath(projects, a.workingDir) {
			currentProject = a.workingDir
		}
		report, err := a.scanInventory(ctx, cfg.Agents, currentProject, projects)
		if err != nil {
			return nil, err
		}
		hookPlans, err := a.planHooks(ctx, cfg.Agents)
		if err != nil {
			return nil, err
		}
		schedule, err := scheduler.BuildPlan(scheduler.Options{
			Executable: a.executable, Home: a.home, StateDir: paths.StateDir,
			Hour: -1, Minute: -1,
		})
		if err != nil {
			return nil, err
		}

		var persisted inventory.PersistResult
		var bundleInventory bundle.DiscoverResult
		configBeforeHash := fileHashOrEmpty(paths.ConfigFile)
		value := plan.Plan{ID: "init-v2", Title: "Initialize ToolTend v0.2 bundle inventory"}
		candidateJSON, _ := json.Marshal(projectCandidates)
		inventoryPreview := buildInitInventoryPreview(report)
		inventoryJSON, _ := json.Marshal(inventoryPreview)
		value.Operations = append(value.Operations, plan.FuncOperation{
			Description: plan.OperationPreview{
				ID: "select-projects", Kind: plan.OperationOther, Target: "project inventory",
				Summary:              "Use the selected projects and show bounded candidates discovered from agent configuration",
				RequiresConfirmation: true,
				Details: map[string]string{
					"selected":           strings.Join(projects, ","),
					"project_candidates": string(candidateJSON),
				},
			},
		})
		value.Operations = append(value.Operations, plan.FuncOperation{
			Description: plan.OperationPreview{
				ID: "preview-inventory", Kind: plan.OperationOther, Target: "component inventory",
				Summary:              "Show discovery evidence that will be grouped into unconfigured bundles",
				RequiresConfirmation: true,
				Details:              map[string]string{"components": string(inventoryJSON)},
			},
		})
		value.Operations = append(value.Operations,
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "create-owned-directories", Kind: plan.OperationCreateDirectory,
					Target: paths.DataDir, Summary: "Create ToolTend-owned configuration, state, object, staging, generation, runtime, and shim directories",
					RequiresConfirmation: true,
					Details:              map[string]string{"shim_dir": cfg.Runtime.ShimDir},
				},
				ApplyFunc: func(context.Context) error {
					if ensureErr := paths.Ensure(); ensureErr != nil {
						return ensureErr
					}
					return os.MkdirAll(cfg.Runtime.ShimDir, 0o755)
				},
			},
		)
		if profile != nil {
			profile := *profile
			value.Operations = append(value.Operations, plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "configure-shim-path", Kind: plan.OperationWriteFile, Target: profile.Path,
					Summary:    "Add the ToolTend managed shim directory to PATH in the shell profile",
					BeforeHash: profile.BeforeHash, AfterHash: profile.AfterHash, RequiresConfirmation: true,
				},
				ApplyFunc: func(context.Context) error {
					if checkErr := assertFileHash(profile.Path, profile.BeforeHash); checkErr != nil {
						return checkErr
					}
					return safeio.AtomicWriteFile(profile.Path, profile.Content, profile.Mode)
				},
			})
		}
		value.Operations = append(value.Operations,
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "write-config", Kind: plan.OperationWriteFile, Target: paths.ConfigFile,
					Summary:    "Write the selected agents, projects, schedule, notifications, and runtime configuration",
					BeforeHash: configBeforeHash, RequiresConfirmation: true,
				},
				ApplyFunc: func(context.Context) error {
					if err := assertFileHash(paths.ConfigFile, configBeforeHash); err != nil {
						return err
					}
					return config.SaveAtomic(paths.ConfigFile, cfg)
				},
			},
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "initialize-inventory", Kind: plan.OperationDatabase, Target: paths.DatabaseFile,
					Summary:              "Create or migrate the SQLite state database and persist the read-only discovery result",
					RequiresConfirmation: true,
					Details:              map[string]string{"observations": fmt.Sprint(len(report.HostResult.Observations)), "bindings": fmt.Sprint(len(report.HostResult.Bindings)), "bundle_policy": "all discovered bundles remain unconfigured"},
				},
				ApplyFunc: func(ctx context.Context) error {
					database, openErr := store.OpenRW(paths.DatabaseFile)
					if openErr != nil {
						return openErr
					}
					defer database.Close()
					persisted, openErr = inventory.Persist(ctx, database, report)
					if openErr != nil {
						return openErr
					}
					bundleInventory, openErr = bundle.Discover(ctx, database, bundle.DiscoverOptions{
						HomeDir: a.home, Executable: a.executable, BuildVersion: buildinfo.Version,
						LocalRecipeDir: filepath.Join(paths.ConfigDir, "bundles.d"),
						LookupPath:     a.lookupPath,
					})
					return openErr
				},
			},
		)
		for _, hookPlan := range hookPlans {
			for mutationIndex, mutation := range hookPlan.Mutations {
				if !mutation.Changed {
					continue
				}
				mutation := mutation
				value.Operations = append(value.Operations, plan.FuncOperation{
					Description: plan.OperationPreview{
						ID: fmt.Sprintf("install-hook-%s-%d", hookPlan.Host, mutationIndex+1), Kind: plan.OperationInstallHook,
						Target: mutation.Path, Summary: "Install fast fail-open ToolTend hooks for " + string(hookPlan.Host),
						BeforeHash: mutation.BeforeSHA256, AfterHash: mutation.AfterSHA256,
						RequiresConfirmation: true,
					},
					ApplyFunc: func(context.Context) error { return host.ApplyMutation(mutation) },
				})
			}
		}
		scheduleDetails := make(map[string]string, len(schedule.Files)*2)
		scheduleBefore := make(map[string]string, len(schedule.Files))
		var scheduleTargets []string
		for index, file := range schedule.Files {
			scheduleTargets = append(scheduleTargets, file.Path)
			scheduleBefore[file.Path] = fileHashOrEmpty(file.Path)
			scheduleDetails[fmt.Sprintf("file_%d_before", index+1)] = scheduleBefore[file.Path]
			scheduleDetails[fmt.Sprintf("file_%d_after", index+1)] = contentHash(file.Content)
		}
		value.Operations = append(value.Operations,
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "install-schedule", Kind: plan.OperationInstallSchedule,
					Target: strings.Join(scheduleTargets, ","), Summary: "Install the daily one-shot " + schedule.Platform + " schedule files",
					RequiresConfirmation: true, Details: scheduleDetails,
				},
				ApplyFunc: func(context.Context) error {
					for _, file := range schedule.Files {
						if err := assertFileHash(file.Path, scheduleBefore[file.Path]); err != nil {
							return err
						}
					}
					return scheduler.Apply(schedule)
				},
			},
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "activate-schedule", Kind: plan.OperationInstallSchedule,
					Target: schedule.Platform, Summary: "Register and enable the daily one-shot schedule with the operating system",
					RequiresConfirmation: true,
				},
				ApplyFunc: func(ctx context.Context) error {
					if activateErr := scheduler.Activate(ctx, schedule, a.runner); activateErr != nil {
						return &commandError{Code: "scheduler_activation_failed", Message: "schedule files were written but the operating-system job could not be activated", Cause: activateErr}
					}
					return nil
				},
			},
		)
		return a.applyPlan(ctx, value, func() any {
			result := map[string]any{
				"paths": paths, "inventory": persisted, "bundles": bundleInventory,
				"project_candidates": projectCandidates, "warnings": report.HostResult.Warnings,
				"next_command": "tooltend bundles configure",
			}
			return result
		})
	})
	return command
}

func containsPath(values []string, target string) bool {
	target = filepath.Clean(target)
	for _, value := range values {
		if filepath.Clean(value) == target {
			return true
		}
	}
	return false
}

func buildInitInventoryPreview(report inventory.Report) []initInventoryItem {
	observations := make(map[string]host.Observation, len(report.HostResult.Observations))
	type group struct {
		copies   int
		versions map[string]struct{}
	}
	groups := make(map[string]*group)
	for _, observation := range report.HostResult.Observations {
		observations[observation.Key] = observation
		identity := sourcePreviewIdentity(observation.Source, observation.Kind, observation.Name)
		value := groups[identity]
		if value == nil {
			value = &group{versions: map[string]struct{}{}}
			groups[identity] = value
		}
		value.copies++
		if version := observationVersion(observation); version != "" {
			value.versions[version] = struct{}{}
		}
	}
	result := make([]initInventoryItem, 0, len(report.HostResult.Bindings))
	for _, binding := range report.HostResult.Bindings {
		observation, ok := observations[binding.ComponentKey]
		if !ok {
			continue
		}
		classification := previewClassification(observation)
		mode := model.ApplyManual
		if classification == model.ClassificationClean && strings.EqualFold(observation.Source.Kind, "git") && observation.Kind != host.ComponentHook {
			mode = model.ApplyAuto
		}
		identity := sourcePreviewIdentity(observation.Source, observation.Kind, observation.Name)
		copies, drift := 1, false
		if grouped := groups[identity]; grouped != nil {
			copies, drift = grouped.copies, len(grouped.versions) > 1
		}
		installPath := binding.InstallPath
		if installPath == "" {
			installPath = observation.Path
		}
		result = append(result, initInventoryItem{
			Host: observation.Host, Kind: observation.Kind, Name: observation.Name,
			Version: observationVersion(observation), InstallPath: installPath,
			Project: binding.Project, Source: observation.Source,
			Classification: classification, RecommendedMode: mode,
			DuplicateCopies: copies, VersionDrift: drift,
		})
	}
	return result
}

var initExactRuntimeVersion = regexp.MustCompile(`^[vV]?[0-9]+(?:\.[0-9]+){0,3}(?:[-+][0-9A-Za-z.-]+)?$`)

func countInitRuntimeMigrationCandidates(report inventory.Report) int {
	observations := make(map[string]host.Observation, len(report.HostResult.Observations))
	for _, observation := range report.HostResult.Observations {
		observations[observation.Key] = observation
	}
	seen := make(map[string]struct{})
	for _, binding := range report.HostResult.Bindings {
		observation, ok := observations[binding.ComponentKey]
		if !ok || (observation.Kind != host.ComponentStdioMCP && observation.Kind != host.ComponentKind("cli")) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(observation.Source.Kind)) {
		case "npm", "npx", "pypi", "pipx", "uv", "uvx", "python":
		default:
			continue
		}
		if strings.TrimSpace(observation.Source.Package) == "" || !initExactRuntimeVersion.MatchString(observationVersion(observation)) {
			continue
		}
		identity := string(binding.Host) + "\x00"
		if observation.Kind == host.ComponentStdioMCP {
			pointer := ""
			if len(observation.Evidence) > 0 {
				pointer = observation.Evidence[0].Pointer
			}
			if binding.ConfigPath == "" || pointer == "" {
				continue
			}
			identity += binding.ConfigPath + "#" + pointer
		} else {
			installPath := binding.InstallPath
			if installPath == "" {
				installPath = observation.Path
			}
			if !filepath.IsAbs(installPath) {
				continue
			}
			identity += filepath.Clean(installPath)
		}
		seen[identity] = struct{}{}
	}
	for _, observation := range report.HostResult.Observations {
		for _, dependency := range observation.Dependencies {
			switch strings.ToLower(strings.TrimSpace(dependency.Source.Kind)) {
			case "npm", "npx", "pypi", "pipx", "uv", "uvx", "python":
			default:
				continue
			}
			if strings.TrimSpace(dependency.Source.Package) == "" ||
				!initExactRuntimeVersion.MatchString(strings.TrimSpace(dependency.Source.Version)) ||
				!filepath.IsAbs(dependency.InstallPath) {
				continue
			}
			seen[string(observation.Host)+"\x00"+filepath.Clean(dependency.InstallPath)] = struct{}{}
		}
	}
	return len(seen)
}

func sourcePreviewIdentity(source host.SourceRef, kind host.ComponentKind, name string) string {
	return strings.Join([]string{strings.ToLower(source.Kind), safeSourceForPreview(source.Locator), source.Subdir, strings.ToLower(source.Package), string(kind), name}, "\x00")
}

func observationVersion(observation host.Observation) string {
	for _, value := range []string{observation.Version, observation.Source.Version, observation.Source.Ref} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func previewClassification(observation host.Observation) model.Classification {
	switch strings.ToLower(observation.Source.Kind) {
	case "local", "path":
		return model.ClassificationDetached
	case "", "unknown":
		return model.ClassificationUnknown
	case "http", "https", "remote":
		return model.ClassificationClean
	default:
		if observationVersion(observation) != "" {
			return model.ClassificationClean
		}
		return model.ClassificationUnknown
	}
}

func (a *App) planShimPath(paths config.Paths) (string, *profileMutation, error) {
	resolvedHome, err := filepath.EvalSymlinks(a.home)
	if err != nil {
		resolvedHome = a.home
	}
	for _, entry := range filepath.SplitList(a.getenv("PATH")) {
		entry = filepath.Clean(strings.TrimSpace(entry))
		// Reuse an existing directory only when it is already the first safe
		// PATH entry. A later ~/.local/bin cannot take over a native command in
		// /usr/local/bin or Homebrew, so init must instead preview a prepend.
		if entry == "." || !filepath.IsAbs(entry) || !pathWithin(entry, a.home) {
			break
		}
		resolved, err := filepath.EvalSymlinks(entry)
		if err != nil || !pathWithin(resolved, resolvedHome) {
			break
		}
		info, err := os.Stat(resolved)
		if err == nil && info.IsDir() && unix.Access(resolved, unix.W_OK) == nil {
			return entry, nil, nil
		}
		break
	}

	shimDir := paths.ShimDir
	if strings.ContainsAny(shimDir, "\x00\r\n") {
		return "", nil, errors.New("shim directory contains invalid characters")
	}
	profilePath := filepath.Join(a.home, ".profile")
	if filepath.Base(strings.TrimSpace(a.getenv("SHELL"))) == "zsh" {
		profilePath = filepath.Join(a.home, ".zprofile")
	}
	content, mode, err := readProfileForPlan(profilePath)
	if err != nil {
		return "", nil, err
	}
	line := "export PATH=" + shellSingleQuote(shimDir) + `:"$PATH" # ToolTend`
	updated := replaceToolTendPATHLine(content, line)
	if string(updated) == string(content) {
		return shimDir, nil, nil
	}
	return shimDir, &profileMutation{
		Path: profilePath, Content: updated, Mode: mode,
		BeforeHash: fileHashOrEmpty(profilePath), AfterHash: contentHash(updated),
	}, nil
}

func pathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func readProfileForPlan(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o644, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("shell profile must be a regular file: %s", path)
	}
	content, err := os.ReadFile(path)
	return content, info.Mode().Perm(), err
}

func replaceToolTendPATHLine(content []byte, desired string) []byte {
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	found := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "export PATH=") && strings.HasSuffix(trimmed, "# ToolTend") {
			lines[index], found = desired, true
		}
	}
	if !found {
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		lines = append(lines, desired)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (a *App) scanInventory(ctx context.Context, agents []model.HostKind, currentProject string, projects []string) (inventory.Report, error) {
	adapters := make([]host.Host, 0, len(agents))
	for _, agent := range agents {
		switch agent {
		case model.HostCodex:
			adapters = append(adapters, host.NewCodex())
		case model.HostClaude:
			adapters = append(adapters, host.NewClaude())
		default:
			return inventory.Report{}, cliError("invalid_configuration", "configured agent must be codex or claude", nil)
		}
	}
	if len(adapters) == 0 {
		return inventory.Report{}, cliError("invalid_configuration", "at least one agent must be selected", nil)
	}
	result, err := host.ScanAll(ctx, host.ScanOptions{
		HomeDir: a.home, CurrentProject: currentProject, Projects: projects,
	}, adapters...)
	if err != nil {
		return inventory.Report{}, err
	}
	selected := append([]string(nil), projects...)
	if currentProject != "" && !containsPath(selected, currentProject) {
		selected = append(selected, currentProject)
	}
	return inventory.Report{HostResult: result, Projects: selected}, nil
}

func (a *App) initProjects(existing, requested []string) ([]string, error) {
	values := append([]string(nil), requested...)
	if len(values) == 0 {
		values = append(values, existing...)
		if a.workingDir != "" {
			values = append(values, a.workingDir)
		}
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		absolute, err := a.absolute(value)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[absolute]; ok {
			continue
		}
		seen[absolute] = struct{}{}
		result = append(result, absolute)
	}
	sort.Strings(result)
	return result, nil
}

func parseAgents(values []string) ([]model.HostKind, error) {
	seen := map[model.HostKind]struct{}{}
	result := make([]model.HostKind, 0, len(values))
	for _, raw := range values {
		for _, item := range strings.Split(raw, ",") {
			value := model.HostKind(strings.ToLower(strings.TrimSpace(item)))
			if value != model.HostCodex && value != model.HostClaude {
				return nil, cliError("invalid_argument", "agent must be codex or claude", nil)
			}
			if _, exists := seen[value]; exists {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result, nil
}

func (a *App) planHooks(ctx context.Context, agents []model.HostKind) ([]host.MutationPlan, error) {
	var result []host.MutationPlan
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
		value, err := adapter.PlanHookInstall(ctx, host.HookInstallOptions{
			HomeDir: a.home, BinaryPath: a.executable, Scope: host.ScopeUser,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func contentHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func fileHashOrEmpty(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return contentHash(data)
}

func assertFileHash(path, expected string) error {
	data, err := os.ReadFile(path)
	if expected == "" && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("target changed after preview: %s: %w", path, err)
	}
	if contentHash(data) != expected {
		return fmt.Errorf("target changed after preview: %s", path)
	}
	return nil
}

func (a *App) newScanCommand() *cobra.Command {
	var projects []string
	command := &cobra.Command{Use: "scan", Short: "Reconcile the local extension inventory", Args: cobra.NoArgs}
	command.Flags().StringSliceVar(&projects, "project", nil, "scan an explicitly selected project")
	command.RunE = a.run("scan", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		cfg, err := config.Load(paths.ConfigFile)
		if err != nil {
			return nil, err
		}
		initialized, err := a.openReadOnly(paths)
		if err != nil {
			return nil, err
		}
		if closeErr := initialized.Close(); closeErr != nil {
			return nil, closeErr
		}
		selected, err := a.initProjects(cfg.Projects, projects)
		if err != nil {
			return nil, err
		}
		currentProject := ""
		if containsPath(selected, a.workingDir) {
			currentProject = a.workingDir
		}
		report, err := a.scanInventory(ctx, cfg.Agents, currentProject, selected)
		if err != nil {
			return nil, err
		}
		var persisted inventory.PersistResult
		var bundleInventory bundle.DiscoverResult
		value := plan.Plan{ID: "scan-v1", Title: "Persist the current ToolTend inventory", Operations: []plan.Operation{
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "persist-inventory", Kind: plan.OperationDatabase, Target: paths.DatabaseFile,
					Summary:              "Persist normalized components and bindings from the read-only host scan",
					RequiresConfirmation: true,
					Details:              map[string]string{"observations": fmt.Sprint(len(report.HostResult.Observations)), "bindings": fmt.Sprint(len(report.HostResult.Bindings))},
				},
				ApplyFunc: func(ctx context.Context) error {
					return withLifecycleStateLock(ctx, paths, func(database *store.Store) error {
						var persistErr error
						persisted, persistErr = inventory.Persist(ctx, database, report)
						if persistErr != nil {
							return persistErr
						}
						bundleInventory, persistErr = bundle.Discover(ctx, database, bundle.DiscoverOptions{
							HomeDir: a.home, Executable: a.executable, BuildVersion: buildinfo.Version,
							LocalRecipeDir: filepath.Join(paths.ConfigDir, "bundles.d"),
							LookupPath:     a.lookupPath,
						})
						return persistErr
					})
				},
			},
		}}
		return a.applyPlan(ctx, value, func() any {
			return map[string]any{"inventory": persisted, "bundles": bundleInventory, "warnings": report.HostResult.Warnings}
		})
	})
	return command
}

func (a *App) lookupPath(name string) (string, error) {
	if name == "" || filepath.Base(name) != name {
		return "", os.ErrNotExist
	}
	for _, directory := range filepath.SplitList(a.getenv("PATH")) {
		if directory == "" || !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}
