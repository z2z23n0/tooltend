package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/project"
	"github.com/z2z23n0/tooltend/internal/store"
)

func (a *App) newProjectCommand() *cobra.Command {
	parent := &cobra.Command{Use: "project", Short: "Manage reproducible project manifests and locks", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	parent.AddCommand(a.newProjectInitCommand(), a.newProjectExportCommand(), a.newProjectSyncCommand())
	return parent
}

func (a *App) newProjectInitCommand() *cobra.Command {
	var root string
	command := &cobra.Command{Use: "init", Short: "Create an empty tooltend.toml and tooltend.lock", Args: cobra.NoArgs}
	command.Flags().StringVar(&root, "root", "", "project root (defaults to the current directory)")
	command.RunE = a.run("project init", func(ctx context.Context) (any, error) {
		projectRoot, err := a.projectRoot(root)
		if err != nil {
			return nil, err
		}
		var result project.ExportResult
		manifestPath, lockPath := filepath.Join(projectRoot, project.ManifestName), filepath.Join(projectRoot, project.LockName)
		manifestBefore, lockBefore := fileHashOrEmpty(manifestPath), fileHashOrEmpty(lockPath)
		value := plan.Plan{ID: "project-init-v1", Title: "Initialize ToolTend project files", Operations: []plan.Operation{
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "write-project-manifest-and-lock", Kind: plan.OperationWriteFile, Target: projectRoot,
					Summary:              "Write an empty versioned tooltend.toml and tooltend.lock",
					RequiresConfirmation: true,
					Details: map[string]string{
						"manifest_before_hash": manifestBefore,
						"lock_before_hash":     lockBefore,
					},
				},
				ApplyFunc: func(context.Context) error {
					if checkErr := assertFileHash(manifestPath, manifestBefore); checkErr != nil {
						return checkErr
					}
					if checkErr := assertFileHash(lockPath, lockBefore); checkErr != nil {
						return checkErr
					}
					var initErr error
					result, initErr = project.Init(projectRoot)
					return initErr
				},
			},
		}}
		return a.applyPlan(ctx, value, func() any { return result })
	})
	return command
}

func (a *App) newProjectExportCommand() *cobra.Command {
	var root string
	command := &cobra.Command{Use: "export", Short: "Export selected bindings to the project manifest and lock", Args: cobra.NoArgs}
	command.Flags().StringVar(&root, "root", "", "project root (defaults to the current directory)")
	command.RunE = a.run("project export", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		projectRoot, err := a.projectRoot(root)
		if err != nil {
			return nil, err
		}
		manifestPath, lockPath := filepath.Join(projectRoot, project.ManifestName), filepath.Join(projectRoot, project.LockName)
		manifestBefore, lockBefore := fileHashOrEmpty(manifestPath), fileHashOrEmpty(lockPath)
		database, err := a.openReadOnly(paths)
		if err != nil {
			return nil, err
		}
		result, err := project.Export(ctx, database, projectRoot)
		database.Close()
		if err != nil {
			return nil, err
		}
		if err := assertFileHash(manifestPath, manifestBefore); err != nil {
			return nil, err
		}
		if err := assertFileHash(lockPath, lockBefore); err != nil {
			return nil, err
		}
		value := plan.Plan{ID: "project-export-v1", Title: "Export ToolTend project state", Operations: []plan.Operation{
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "write-project-export", Kind: plan.OperationWriteFile, Target: projectRoot,
					Summary:              "Write source and version targets to tooltend.toml and verified resolutions to tooltend.lock",
					RequiresConfirmation: true,
					Details: map[string]string{
						"manifest_before_hash": manifestBefore,
						"lock_before_hash":     lockBefore,
					},
				},
				ApplyFunc: func(context.Context) error {
					if checkErr := assertFileHash(manifestPath, manifestBefore); checkErr != nil {
						return checkErr
					}
					if checkErr := assertFileHash(lockPath, lockBefore); checkErr != nil {
						return checkErr
					}
					return project.WriteExport(projectRoot, result)
				},
			},
		}}
		return a.applyPlan(ctx, value, func() any { return result })
	})
	return command
}

func (a *App) newProjectSyncCommand() *cobra.Command {
	var root string
	command := &cobra.Command{Use: "sync", Short: "Apply project version targets without elevating local trust or policy", Args: cobra.NoArgs}
	command.Flags().StringVar(&root, "root", "", "project root (defaults to the current directory)")
	command.RunE = a.run("project sync", func(ctx context.Context) (any, error) {
		paths, err := a.paths()
		if err != nil {
			return nil, err
		}
		projectRoot, err := a.projectRoot(root)
		if err != nil {
			return nil, err
		}
		manifestPath, lockPath := filepath.Join(projectRoot, project.ManifestName), filepath.Join(projectRoot, project.LockName)
		manifestBefore, lockBefore := fileHashOrEmpty(manifestPath), fileHashOrEmpty(lockPath)
		database, err := a.openReadOnly(paths)
		if err != nil {
			return nil, err
		}
		preview, err := project.Sync(ctx, database, projectRoot, false)
		database.Close()
		if err != nil {
			return nil, err
		}
		if err := assertFileHash(manifestPath, manifestBefore); err != nil {
			return nil, err
		}
		if err := assertFileHash(lockPath, lockBefore); err != nil {
			return nil, err
		}
		changesJSON, err := json.Marshal(preview.Changes)
		if err != nil {
			return nil, err
		}
		warningsJSON, err := json.Marshal(preview.Warnings)
		if err != nil {
			return nil, err
		}
		var applied project.SyncResult
		value := plan.Plan{ID: "project-sync-v1", Title: "Synchronize ToolTend project targets", Operations: []plan.Operation{
			plan.FuncOperation{
				Description: plan.OperationPreview{
					ID: "sync-project-targets", Kind: plan.OperationDatabase, Target: paths.DatabaseFile,
					Summary:              "Apply only non-elevating version-target changes to already observed bindings",
					RequiresConfirmation: true,
					Details: map[string]string{
						"changes": integerString(len(preview.Changes)), "warnings": integerString(len(preview.Warnings)),
						"changes_json": string(changesJSON), "warnings_json": string(warningsJSON),
						"manifest_hash": manifestBefore, "lock_hash": lockBefore,
						"policy_snapshot_hash": projectPolicySnapshotHash(preview.PolicySnapshots),
					},
				},
				ApplyFunc: func(ctx context.Context) error {
					if checkErr := assertFileHash(manifestPath, manifestBefore); checkErr != nil {
						return checkErr
					}
					if checkErr := assertFileHash(lockPath, lockBefore); checkErr != nil {
						return checkErr
					}
					openErr := withLifecycleStateLock(ctx, paths, func(db *store.Store) error {
						var applyErr error
						applied, applyErr = project.ApplySyncPreview(ctx, db, preview)
						return applyErr
					})
					if errors.Is(openErr, project.ErrSyncStateChanged) {
						return cliError("state_changed", "binding policy changed after project sync preview", openErr)
					}
					return openErr
				},
			},
		}}
		return a.applyPlan(ctx, value, func() any { return applied })
	})
	return command
}

func (a *App) projectRoot(value string) (string, error) {
	if value == "" {
		value = a.workingDir
	}
	return a.absolute(value)
}

func projectPolicySnapshotHash(values []project.PolicySnapshot) string {
	encoded, _ := json.Marshal(values)
	return contentHash(encoded)
}

func integerString(value int) string { return fmt.Sprint(value) }
