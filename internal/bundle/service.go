package bundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type Service struct {
	Database *store.Store
	Paths    config.Paths
	Runner   execx.Runner
	Now      func() time.Time
}

type UpdatePreview struct {
	Bundle       model.Bundle            `json:"bundle"`
	Policy       model.BundlePolicy      `json:"policy"`
	Current      *model.BundleRelease    `json:"current_release,omitempty"`
	Target       model.BundleRelease     `json:"target_release"`
	Artifacts    []UpdateArtifactPreview `json:"artifacts"`
	StageOnly    bool                    `json:"stage_only"`
	AutoEligible bool                    `json:"auto_eligible"`
}

type UpdateArtifactPreview struct {
	Artifact        model.BundleArtifact `json:"artifact"`
	Installations   int                  `json:"installations"`
	ResolvedVersion string               `json:"resolved_version,omitempty"`
	CanStage        bool                 `json:"can_stage"`
	CanActivate     bool                 `json:"can_activate"`
	CanRollback     bool                 `json:"can_rollback"`
	CanHealthCheck  bool                 `json:"can_health_check"`
}

type UpdateResult struct {
	Transaction model.BundleTransaction `json:"transaction"`
	Release     model.BundleRelease     `json:"release"`
	Receipt     *model.BundleReceipt    `json:"receipt,omitempty"`
}

type RollbackPreview struct {
	Bundle model.Bundle        `json:"bundle"`
	Policy model.BundlePolicy  `json:"policy"`
	From   model.BundleRelease `json:"from_release"`
	To     model.BundleRelease `json:"to_release"`
	Steps  int                 `json:"steps"`
}

type RollbackResult struct {
	Transaction model.BundleTransaction `json:"transaction"`
	Release     model.BundleRelease     `json:"release"`
	Receipt     model.BundleReceipt     `json:"receipt"`
}

func (s Service) PrepareUpdate(ctx context.Context, bundleID string, stageOnly bool) (UpdatePreview, error) {
	if err := s.validate(); err != nil {
		return UpdatePreview{}, err
	}
	bundleValue, err := s.Database.GetBundle(ctx, bundleID)
	if err != nil {
		return UpdatePreview{}, err
	}
	policy, err := s.Database.GetBundlePolicy(ctx, bundleID)
	if errors.Is(err, sql.ErrNoRows) || bundleValue.ConfigState != model.BundleConfigured {
		return UpdatePreview{}, errors.New("bundle is unconfigured; run tooltend bundles configure first")
	}
	if err != nil {
		return UpdatePreview{}, err
	}
	if policy.Mode == model.BundlePolicyObserve || policy.Mode == model.BundlePolicyIgnore {
		return UpdatePreview{}, fmt.Errorf("bundle policy %s is observation-only", policy.Mode)
	}
	if bundleValue.Owner != model.LifecycleToolTend && bundleValue.Owner != model.LifecycleDelegated {
		return UpdatePreview{}, fmt.Errorf("bundle lifecycle owner %s is observation-only", bundleValue.Owner)
	}
	artifacts, err := s.Database.ListBundleArtifacts(ctx, bundleID)
	if err != nil {
		return UpdatePreview{}, err
	}
	installations, err := s.Database.ListInstallations(ctx, bundleID)
	if err != nil {
		return UpdatePreview{}, err
	}
	byArtifact := make(map[string][]model.Installation)
	for _, installation := range installations {
		byArtifact[installation.ArtifactID] = append(byArtifact[installation.ArtifactID], installation)
	}
	preview := UpdatePreview{Bundle: bundleValue, Policy: policy, StageOnly: stageOnly, AutoEligible: true}
	if bundleValue.CurrentReleaseID != "" {
		current, currentErr := s.Database.GetBundleRelease(ctx, bundleValue.CurrentReleaseID)
		if currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows) {
			return UpdatePreview{}, currentErr
		}
		if currentErr == nil {
			preview.Current = &current
		}
	}
	versions := map[string]string{}
	for _, artifact := range artifacts {
		recipe, err := decodeArtifactMetadata(artifact)
		if err != nil {
			return UpdatePreview{}, err
		}
		item := UpdateArtifactPreview{
			Artifact: artifact, Installations: len(byArtifact[artifact.ID]), CanStage: len(recipe.StageArgv) > 0,
			CanActivate: len(recipe.ActivateArgv) > 0, CanRollback: len(recipe.RollbackArgv) > 0, CanHealthCheck: len(recipe.HealthArgv) > 0,
		}
		if len(byArtifact[artifact.ID]) == 0 {
			if artifact.Required {
				return UpdatePreview{}, fmt.Errorf("required bundle artifact %s has no physical installation", artifact.Name)
			}
			preview.Artifacts = append(preview.Artifacts, item)
			continue
		}
		if len(recipe.ResolveArgv) == 0 {
			preview.AutoEligible = false
			return UpdatePreview{}, fmt.Errorf("installed bundle artifact %s has no release resolver", artifact.Name)
		}
		resolved, err := s.runResolver(ctx, recipe.ResolveArgv, firstInstallation(byArtifact[artifact.ID]))
		if err != nil {
			return UpdatePreview{}, fmt.Errorf("resolve artifact %s: %w", artifact.Name, err)
		}
		item.ResolvedVersion = resolved
		versions[artifact.RecipeKey] = resolved
		if !item.CanStage || !item.CanActivate || !item.CanRollback || !item.CanHealthCheck {
			preview.AutoEligible = false
		}
		if !item.CanStage {
			return UpdatePreview{}, fmt.Errorf("installed bundle artifact %s has no staging command", artifact.Name)
		}
		if !stageOnly && (!item.CanActivate || !item.CanHealthCheck) {
			return UpdatePreview{}, fmt.Errorf("installed bundle artifact %s has no activation or health command", artifact.Name)
		}
		preview.Artifacts = append(preview.Artifacts, item)
	}
	if len(versions) == 0 {
		return UpdatePreview{}, errors.New("bundle has no resolvable artifacts")
	}
	if policy.Mode == model.BundlePolicyAuto && !preview.AutoEligible {
		return UpdatePreview{}, errors.New("bundle recipe is not eligible for auto updates because staging, rollback, or health evidence is incomplete")
	}
	manifest, _ := json.Marshal(map[string]any{"artifacts": versions})
	releaseVersion := bundleReleaseVersion(versions, manifest)
	preview.Target = model.BundleRelease{
		ID: stableID("rel", bundleValue.ID+"\x00"+string(manifest)), BundleID: bundleValue.ID, Version: releaseVersion,
		ResolvedRef: releaseVersion, ManifestJSON: string(manifest), Status: "resolved", CreatedAt: s.now(),
	}
	return preview, nil
}

func (s Service) ExecuteUpdate(ctx context.Context, preview UpdatePreview) (result UpdateResult, err error) {
	if err := s.validate(); err != nil {
		return result, err
	}
	currentBundle, err := s.Database.GetBundle(ctx, preview.Bundle.ID)
	if err != nil {
		return result, err
	}
	currentPolicy, err := s.Database.GetBundlePolicy(ctx, preview.Bundle.ID)
	if err != nil {
		return result, err
	}
	if currentBundle.LastSeenAt != preview.Bundle.LastSeenAt || currentBundle.ConfigState != model.BundleConfigured || currentPolicy.UpdatedAt != preview.Policy.UpdatedAt || currentPolicy.Mode != preview.Policy.Mode {
		return result, errors.New("bundle or policy changed after update preview")
	}
	if err := os.MkdirAll(s.Paths.StagingDir, 0o700); err != nil {
		return result, err
	}
	transactionID, err := model.NewID("btx")
	if err != nil {
		return result, err
	}
	now := s.now()
	transaction := model.BundleTransaction{
		ID: transactionID, BundleID: preview.Bundle.ID, ToReleaseID: preview.Target.ID, Status: model.BundleTransactionPrepared,
		StageOnly: preview.StageOnly, StartedAt: now, UpdatedAt: now,
	}
	if preview.Current != nil {
		transaction.FromReleaseID = preview.Current.ID
	}
	if err := s.Database.UpsertBundleRelease(ctx, preview.Target); err != nil {
		return result, err
	}
	if err := s.Database.PutBundleTransaction(ctx, transaction); err != nil {
		return result, err
	}
	result.Transaction, result.Release = transaction, preview.Target
	stageRoot := filepath.Join(s.Paths.StagingDir, transactionID)
	if err := os.MkdirAll(stageRoot, 0o700); err != nil {
		return result, s.failTransaction(ctx, transaction, "staging_failed", err)
	}
	defer os.RemoveAll(stageRoot)
	artifacts, err := s.Database.ListBundleArtifacts(ctx, preview.Bundle.ID)
	if err != nil {
		return result, s.failTransaction(ctx, transaction, "inventory_changed", err)
	}
	installations, err := s.Database.ListInstallations(ctx, preview.Bundle.ID)
	if err != nil {
		return result, s.failTransaction(ctx, transaction, "inventory_changed", err)
	}
	byArtifact := map[string][]model.Installation{}
	for _, installation := range installations {
		byArtifact[installation.ArtifactID] = append(byArtifact[installation.ArtifactID], installation)
	}
	versions := releaseVersions(preview.Target.ManifestJSON)
	steps := []executionStep{}
	ordinal := 0
	for _, artifact := range artifacts {
		recipe, decodeErr := decodeArtifactMetadata(artifact)
		if decodeErr != nil {
			return result, s.failTransaction(ctx, transaction, "invalid_recipe", decodeErr)
		}
		for _, installation := range byArtifact[artifact.ID] {
			if len(recipe.StageArgv) == 0 && len(recipe.ActivateArgv) == 0 {
				continue
			}
			stepID := stableID("bst", transactionID+fmt.Sprintf("\x00%d", ordinal))
			step := executionStep{
				record: model.BundleTransactionStep{ID: stepID, TransactionID: transactionID, Ordinal: ordinal, ArtifactID: artifact.ID,
					InstallationID: installation.ID, Kind: string(artifact.Kind), Status: model.BundleStepPending, CommandJSON: "[]", RollbackJSON: "[]", BeforeJSON: "{}", AfterJSON: "{}"},
				artifact: artifact, installation: installation, recipe: recipe, version: versions[artifact.RecipeKey],
				rollbackVersion: installation.ObservedVersion, stagePath: filepath.Join(stageRoot, fmt.Sprintf("%03d", ordinal)),
			}
			if err := os.MkdirAll(step.stagePath, 0o700); err != nil {
				return result, s.failTransaction(ctx, transaction, "staging_failed", err)
			}
			step.record.CommandJSON = commandEvidence(recipe.StageArgv)
			step.record.RollbackJSON = commandEvidence(recipe.RollbackArgv)
			if err := s.Database.PutBundleTransactionStep(ctx, step.record); err != nil {
				return result, s.failTransaction(ctx, transaction, "journal_failed", err)
			}
			steps = append(steps, step)
			ordinal++
		}
	}
	if len(steps) == 0 {
		return result, s.failTransaction(ctx, transaction, "no_update_driver", errors.New("bundle recipe has no executable update steps"))
	}
	if err := s.Database.UpdateBundleTransaction(ctx, transaction.ID, model.BundleTransactionStaging, "", "", nil); err != nil {
		return result, err
	}
	transaction.Status = model.BundleTransactionStaging
	for index := range steps {
		step := &steps[index]
		if len(step.recipe.StageArgv) > 0 {
			if err := s.runCommand(ctx, step.recipe.StageArgv, *step, DefaultInstallTimeout); err != nil {
				_ = s.failStep(ctx, step.record.ID, "staging_failed")
				return result, s.failTransaction(ctx, transaction, "staging_failed", err)
			}
		}
		completed := s.now()
		step.record.Status = model.BundleStepStaged
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepStaged, "", "", "{}", &completed); err != nil {
			return result, s.failTransaction(ctx, transaction, "journal_failed", err)
		}
	}
	if preview.StageOnly {
		completed := s.now()
		if err := s.Database.CommitStagedBundleTransaction(ctx, transaction.ID, preview.Target.ID, completed); err != nil {
			return result, err
		}
		transaction.Status, transaction.CompletedAt = model.BundleTransactionCommitted, &completed
		preview.Target.Status = "staged"
		result.Release = preview.Target
		result.Transaction = transaction
		return result, nil
	}
	if err := s.Database.UpdateBundleTransaction(ctx, transaction.ID, model.BundleTransactionActivating, "", "", nil); err != nil {
		return result, err
	}
	transaction.Status = model.BundleTransactionActivating
	activated := []executionStep{}
	for index := range steps {
		step := &steps[index]
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepActivating, "", "", "{}", nil); err != nil {
			return result, s.failTransaction(ctx, transaction, "journal_failed", err)
		}
		if err := s.runCommand(ctx, step.recipe.ActivateArgv, *step, DefaultInstallTimeout); err != nil {
			_ = s.failStep(ctx, step.record.ID, "activation_failed")
			rollbackSteps := append(append([]executionStep(nil), activated...), *step)
			rollbackErr := s.compensate(ctx, transaction, rollbackSteps)
			if rollbackErr == nil {
				return result, s.recordRolledBack(ctx, transaction, "activation_failed", err)
			}
			return result, s.failTransaction(ctx, transaction, "activation_failed", errors.Join(err, rollbackErr))
		}
		completed := s.now()
		step.record.Status = model.BundleStepActivated
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepActivated, "", "", "{}", &completed); err != nil {
			return result, s.failTransaction(ctx, transaction, "journal_failed", err)
		}
		activated = append(activated, *step)
	}
	for index := range activated {
		step := &activated[index]
		if len(step.recipe.HealthArgv) == 0 {
			continue
		}
		if err := s.runCommand(ctx, step.recipe.HealthArgv, *step, DefaultHealthTimeout); err != nil {
			_ = s.failStep(ctx, step.record.ID, "health_failed")
			rollbackErr := s.compensate(ctx, transaction, activated)
			if rollbackErr == nil {
				return result, s.recordRolledBack(ctx, transaction, "health_failed", err)
			}
			return result, s.failTransaction(ctx, transaction, "health_failed", errors.Join(err, rollbackErr))
		}
		checkID, _ := model.NewID("bhc")
		_ = s.Database.PutBundleHealthCheck(ctx, model.BundleHealthCheck{ID: checkID, BundleID: preview.Bundle.ID, ArtifactID: step.artifact.ID,
			InstallationID: step.installation.ID, Name: "recipe-health", Status: "healthy", CheckedAt: s.now()})
		completed := s.now()
		_ = s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepHealthy, "", "", "{}", &completed)
	}
	completed := s.now()
	observations := make([]store.InstallationObservation, 0, len(activated))
	for _, step := range activated {
		observations = append(observations, store.InstallationObservation{InstallationID: step.installation.ID, Version: step.version})
	}
	receiptID, _ := model.NewID("brc")
	receipt := model.BundleReceipt{ID: receiptID, BundleID: preview.Bundle.ID, TransactionID: transaction.ID, ReleaseID: preview.Target.ID,
		Action: "update", Status: "succeeded", SummaryJSON: "{}", CreatedAt: completed}
	fromReleaseID := ""
	if preview.Current != nil {
		fromReleaseID = preview.Current.ID
	}
	if err := s.Database.CommitBundleActivation(ctx, transaction.ID, preview.Bundle.ID, fromReleaseID, preview.Target.ID, observations, receipt, completed); err != nil {
		rollbackErr := s.compensate(ctx, transaction, activated)
		if rollbackErr == nil {
			return result, s.recordRolledBack(ctx, transaction, "commit_failed", err)
		}
		return result, s.failTransaction(ctx, transaction, "commit_failed", errors.Join(err, rollbackErr))
	}
	transaction.Status, transaction.CompletedAt = model.BundleTransactionCommitted, &completed
	preview.Target.Status = "active"
	result.Release = preview.Target
	result.Transaction, result.Receipt = transaction, &receipt
	return result, nil
}

func (s Service) PrepareRollback(ctx context.Context, bundleID, targetReleaseID string) (RollbackPreview, error) {
	if err := s.validate(); err != nil {
		return RollbackPreview{}, err
	}
	bundleValue, err := s.Database.GetBundle(ctx, bundleID)
	if err != nil {
		return RollbackPreview{}, err
	}
	if bundleValue.ConfigState != model.BundleConfigured {
		return RollbackPreview{}, errors.New("bundle is unconfigured; run tooltend bundles configure first")
	}
	policy, err := s.Database.GetBundlePolicy(ctx, bundleID)
	if err != nil {
		return RollbackPreview{}, err
	}
	if policy.Mode == model.BundlePolicyObserve || policy.Mode == model.BundlePolicyIgnore {
		return RollbackPreview{}, fmt.Errorf("bundle policy %s is observation-only", policy.Mode)
	}
	if bundleValue.Owner != model.LifecycleToolTend && bundleValue.Owner != model.LifecycleDelegated {
		return RollbackPreview{}, fmt.Errorf("bundle lifecycle owner %s is observation-only", bundleValue.Owner)
	}
	if bundleValue.CurrentReleaseID == "" {
		return RollbackPreview{}, errors.New("bundle has no current release")
	}
	if targetReleaseID == "" || targetReleaseID == bundleValue.CurrentReleaseID {
		return RollbackPreview{}, errors.New("rollback target must be a prior release")
	}
	from, err := s.Database.GetBundleRelease(ctx, bundleValue.CurrentReleaseID)
	if err != nil {
		return RollbackPreview{}, err
	}
	to, err := s.Database.GetBundleRelease(ctx, targetReleaseID)
	if err != nil {
		return RollbackPreview{}, err
	}
	if to.BundleID != bundleID {
		return RollbackPreview{}, errors.New("rollback target belongs to a different bundle")
	}
	targetVersions, err := parseReleaseVersions(to.ManifestJSON)
	if err != nil {
		return RollbackPreview{}, fmt.Errorf("rollback target manifest: %w", err)
	}
	artifacts, err := s.Database.ListBundleArtifacts(ctx, bundleID)
	if err != nil {
		return RollbackPreview{}, err
	}
	installations, err := s.Database.ListInstallations(ctx, bundleID)
	if err != nil {
		return RollbackPreview{}, err
	}
	byArtifact := make(map[string]int)
	for _, installation := range installations {
		byArtifact[installation.ArtifactID]++
	}
	preview := RollbackPreview{Bundle: bundleValue, Policy: policy, From: from, To: to}
	for _, artifact := range artifacts {
		count := byArtifact[artifact.ID]
		if count == 0 && !artifact.Required {
			continue
		}
		version := targetVersions[artifact.RecipeKey]
		if count > 0 && !exactVersion(version) {
			return RollbackPreview{}, fmt.Errorf("rollback target has no exact version for artifact %s", artifact.Name)
		}
		recipe, err := decodeArtifactMetadata(artifact)
		if err != nil {
			return RollbackPreview{}, err
		}
		if count > 0 && (len(recipe.RollbackArgv) == 0 || len(recipe.ActivateArgv) == 0 || len(recipe.HealthArgv) == 0) {
			return RollbackPreview{}, fmt.Errorf("artifact %s cannot be rolled back safely: rollback, restore, and health commands are required", artifact.Name)
		}
		preview.Steps += count
	}
	if preview.Steps == 0 {
		return RollbackPreview{}, errors.New("bundle has no rollback-capable installations")
	}
	return preview, nil
}

func (s Service) ExecuteRollback(ctx context.Context, preview RollbackPreview) (result RollbackResult, err error) {
	if err := s.validate(); err != nil {
		return result, err
	}
	currentBundle, err := s.Database.GetBundle(ctx, preview.Bundle.ID)
	if err != nil {
		return result, err
	}
	currentPolicy, err := s.Database.GetBundlePolicy(ctx, preview.Bundle.ID)
	if err != nil {
		return result, err
	}
	if currentBundle.CurrentReleaseID != preview.From.ID || currentBundle.LastSeenAt != preview.Bundle.LastSeenAt || currentPolicy.UpdatedAt != preview.Policy.UpdatedAt || currentPolicy.Mode != preview.Policy.Mode {
		return result, errors.New("bundle, policy, or current release changed after rollback preview")
	}
	fromVersions, err := parseReleaseVersions(preview.From.ManifestJSON)
	if err != nil {
		return result, fmt.Errorf("current release manifest: %w", err)
	}
	toVersions, err := parseReleaseVersions(preview.To.ManifestJSON)
	if err != nil {
		return result, fmt.Errorf("rollback release manifest: %w", err)
	}
	transactionID, err := model.NewID("btx")
	if err != nil {
		return result, err
	}
	now := s.now()
	transaction := model.BundleTransaction{ID: transactionID, BundleID: preview.Bundle.ID, FromReleaseID: preview.From.ID,
		ToReleaseID: preview.To.ID, Status: model.BundleTransactionPrepared, StartedAt: now, UpdatedAt: now}
	if err := s.Database.PutBundleTransaction(ctx, transaction); err != nil {
		return result, err
	}
	result.Transaction, result.Release = transaction, preview.To
	artifacts, err := s.Database.ListBundleArtifacts(ctx, preview.Bundle.ID)
	if err != nil {
		return result, s.failTransaction(ctx, transaction, "inventory_changed", err)
	}
	installations, err := s.Database.ListInstallations(ctx, preview.Bundle.ID)
	if err != nil {
		return result, s.failTransaction(ctx, transaction, "inventory_changed", err)
	}
	byArtifact := make(map[string][]model.Installation)
	for _, installation := range installations {
		byArtifact[installation.ArtifactID] = append(byArtifact[installation.ArtifactID], installation)
	}
	var steps []executionStep
	for _, artifact := range artifacts {
		recipe, decodeErr := decodeArtifactMetadata(artifact)
		if decodeErr != nil {
			return result, s.failTransaction(ctx, transaction, "invalid_recipe", decodeErr)
		}
		for _, installation := range byArtifact[artifact.ID] {
			ordinal := len(steps)
			step := executionStep{record: model.BundleTransactionStep{ID: stableID("bst", transactionID+fmt.Sprintf("\x00%d", ordinal)),
				TransactionID: transactionID, Ordinal: ordinal, ArtifactID: artifact.ID, InstallationID: installation.ID,
				Kind: "rollback", Status: model.BundleStepPending, CommandJSON: commandEvidence(recipe.RollbackArgv),
				RollbackJSON: commandEvidence(recipe.ActivateArgv), BeforeJSON: "{}", AfterJSON: "{}"},
				artifact: artifact, installation: installation, recipe: recipe, version: toVersions[artifact.RecipeKey], rollbackVersion: toVersions[artifact.RecipeKey]}
			if err := s.Database.PutBundleTransactionStep(ctx, step.record); err != nil {
				return result, s.failTransaction(ctx, transaction, "journal_failed", err)
			}
			steps = append(steps, step)
		}
	}
	if len(steps) == 0 {
		return result, s.failTransaction(ctx, transaction, "no_rollback_driver", errors.New("bundle has no rollback steps"))
	}
	if err := s.Database.UpdateBundleTransaction(ctx, transaction.ID, model.BundleTransactionRollingBack, "", "", nil); err != nil {
		return result, err
	}
	transaction.Status = model.BundleTransactionRollingBack
	completedSteps := make([]executionStep, 0, len(steps))
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepCompensating, "", "", "{}", nil); err != nil {
			return result, s.failTransaction(ctx, transaction, "journal_failed", err)
		}
		if err := s.runCommand(ctx, step.recipe.RollbackArgv, step, DefaultInstallTimeout); err != nil {
			_ = s.failStep(ctx, step.record.ID, "rollback_failed")
			completedSteps = append(completedSteps, step)
			restoreErr := s.restoreAfterRollbackFailure(ctx, completedSteps, fromVersions)
			return result, s.failTransaction(ctx, transaction, "rollback_failed", errors.Join(err, restoreErr))
		}
		completed := s.now()
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepCompensated, "", "", "{}", &completed); err != nil {
			return result, s.failTransaction(ctx, transaction, "journal_failed", err)
		}
		completedSteps = append(completedSteps, step)
	}
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		if err := s.runCommand(ctx, step.recipe.HealthArgv, step, DefaultHealthTimeout); err != nil {
			restoreErr := s.restoreAfterRollbackFailure(ctx, completedSteps, fromVersions)
			return result, s.failTransaction(ctx, transaction, "rollback_health_failed", errors.Join(err, restoreErr))
		}
		completed := s.now()
		_ = s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepHealthy, "", "", "{}", &completed)
		checkID, _ := model.NewID("bhc")
		_ = s.Database.PutBundleHealthCheck(ctx, model.BundleHealthCheck{ID: checkID, BundleID: preview.Bundle.ID, ArtifactID: step.artifact.ID,
			InstallationID: step.installation.ID, Name: "rollback-health", Status: "healthy", CheckedAt: completed})
	}
	completed := s.now()
	observations := make([]store.InstallationObservation, 0, len(steps))
	for _, step := range steps {
		observations = append(observations, store.InstallationObservation{InstallationID: step.installation.ID, Version: step.version})
	}
	receiptID, _ := model.NewID("brc")
	receipt := model.BundleReceipt{ID: receiptID, BundleID: preview.Bundle.ID, TransactionID: transaction.ID,
		ReleaseID: preview.To.ID, Action: "rollback", Status: "succeeded", SummaryJSON: "{}", CreatedAt: completed}
	if err := s.Database.CommitBundleActivation(ctx, transaction.ID, preview.Bundle.ID, preview.From.ID, preview.To.ID, observations, receipt, completed); err != nil {
		restoreErr := s.restoreAfterRollbackFailure(ctx, completedSteps, fromVersions)
		return result, s.failTransaction(ctx, transaction, "rollback_commit_failed", errors.Join(err, restoreErr))
	}
	transaction.Status, transaction.CompletedAt = model.BundleTransactionCommitted, &completed
	preview.To.Status = "active"
	result.Release = preview.To
	result.Transaction, result.Receipt = transaction, receipt
	return result, nil
}

func (s Service) restoreAfterRollbackFailure(ctx context.Context, completed []executionStep, versions map[string]string) error {
	var failures []error
	for index := len(completed) - 1; index >= 0; index-- {
		step := completed[index]
		step.version = versions[step.artifact.RecipeKey]
		if !exactVersion(step.version) {
			failures = append(failures, fmt.Errorf("artifact %s has no exact restore version", step.artifact.Name))
			continue
		}
		if err := s.runCommand(context.WithoutCancel(ctx), step.recipe.ActivateArgv, step, DefaultInstallTimeout); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

type executionStep struct {
	record          model.BundleTransactionStep
	artifact        model.BundleArtifact
	installation    model.Installation
	recipe          ArtifactRecipe
	version         string
	rollbackVersion string
	stagePath       string
}

func (s Service) runResolver(ctx context.Context, argv []string, installation model.Installation) (string, error) {
	resolved := substituteArgv(argv, map[string]string{"${path}": installation.Path, "${version}": installation.ObservedVersion})
	result, err := s.runWithRetry(ctx, resolved, DefaultResolveTimeout)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(result.Stdout))
	if index := strings.IndexByte(version, '\n'); index >= 0 {
		version = strings.TrimSpace(version[:index])
	}
	if !exactVersion(version) {
		return "", errors.New("resolver did not return an exact semantic version")
	}
	return strings.TrimPrefix(version, "v"), nil
}

func (s Service) runCommand(ctx context.Context, argv []string, step executionStep, timeout time.Duration) error {
	if len(argv) == 0 {
		return nil
	}
	values := map[string]string{
		"${version}": step.version, "${resolved_ref}": step.version, "${stage}": step.stagePath,
		"${path}": step.installation.Path, "${previous_version}": step.installation.ObservedVersion,
		"${rollback_version}": step.rollbackVersion,
	}
	_, err := s.runWithRetry(ctx, substituteArgv(argv, values), timeout)
	return err
}

func (s Service) runWithRetry(ctx context.Context, argv []string, timeout time.Duration) (execx.Result, error) {
	if len(argv) == 0 {
		return execx.Result{}, nil
	}
	var last error
	for attempt := 0; attempt < DefaultRetries; attempt++ {
		commandCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err := s.Runner.Run(commandCtx, argv[0], argv[1:]...)
		cancel()
		if err == nil {
			return result, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return execx.Result{}, fmt.Errorf("driver command failed after %d attempts: %w", DefaultRetries, last)
}

func (s Service) compensate(ctx context.Context, transaction model.BundleTransaction, activated []executionStep) error {
	if err := s.Database.UpdateBundleTransaction(ctx, transaction.ID, model.BundleTransactionRollingBack, "", "", nil); err != nil {
		return err
	}
	var failures []error
	for index := len(activated) - 1; index >= 0; index-- {
		step := activated[index]
		if len(step.recipe.RollbackArgv) == 0 {
			failures = append(failures, fmt.Errorf("artifact %s has no rollback command", step.artifact.Name))
			continue
		}
		if err := s.Database.UpdateBundleTransactionStep(context.WithoutCancel(ctx), step.record.ID, model.BundleStepCompensating, "", "", "{}", nil); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := s.runCommand(context.WithoutCancel(ctx), step.recipe.RollbackArgv, step, DefaultInstallTimeout); err != nil {
			failures = append(failures, err)
			continue
		}
		completed := s.now()
		_ = s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepCompensated, "", "", "{}", &completed)
	}
	return errors.Join(failures...)
}

func (s Service) failTransaction(ctx context.Context, transaction model.BundleTransaction, code string, cause error) error {
	completed := s.now()
	_ = s.Database.UpdateBundleTransaction(ctx, transaction.ID, model.BundleTransactionFailed, code, "", &completed)
	return fmt.Errorf("bundle transaction failed (%s): %w", code, cause)
}

func (s Service) recordRolledBack(ctx context.Context, transaction model.BundleTransaction, code string, cause error) error {
	completed := s.now()
	receiptID, _ := model.NewID("brc")
	receipt := model.BundleReceipt{ID: receiptID, BundleID: transaction.BundleID, TransactionID: transaction.ID,
		ReleaseID: transaction.ToReleaseID, Action: "update", Status: "rolled_back", SummaryJSON: "{}", CreatedAt: completed}
	if err := s.Database.CommitBundleCompensation(ctx, transaction.ID, code, receipt, completed); err != nil {
		return fmt.Errorf("bundle transaction compensation commit failed: %w", err)
	}
	return fmt.Errorf("bundle transaction rolled back (%s): %w", code, cause)
}

func (s Service) failStep(ctx context.Context, id, code string) error {
	completed := s.now()
	return s.Database.UpdateBundleTransactionStep(ctx, id, model.BundleStepFailed, code, "", "{}", &completed)
}

func (s Service) validate() error {
	if s.Database == nil || s.Database.DB() == nil {
		return errors.New("bundle service: database is required")
	}
	if s.Paths.StagingDir == "" {
		return errors.New("bundle service: staging directory is required")
	}
	if s.Runner == nil {
		return errors.New("bundle service: runner is required")
	}
	return nil
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func decodeArtifactMetadata(artifact model.BundleArtifact) (ArtifactRecipe, error) {
	var value ArtifactRecipe
	if err := json.Unmarshal([]byte(artifact.MetadataJSON), &value); err != nil {
		return value, fmt.Errorf("decode recipe for artifact %s: %w", artifact.Name, err)
	}
	if err := value.Kind.Validate(); err != nil {
		return value, err
	}
	for _, argv := range [][]string{value.ResolveArgv, value.StageArgv, value.ActivateArgv, value.RollbackArgv, value.HealthArgv} {
		if err := validateStaticArgv(argv); err != nil {
			return value, err
		}
	}
	return value, nil
}

func firstInstallation(values []model.Installation) model.Installation {
	if len(values) == 0 {
		return model.Installation{}
	}
	return values[0]
}

func substituteArgv(argv []string, values map[string]string) []string {
	result := make([]string, len(argv))
	for index, argument := range argv {
		for variable, value := range values {
			argument = strings.ReplaceAll(argument, variable, value)
		}
		result[index] = argument
	}
	return result
}

func commandEvidence(argv []string) string {
	if len(argv) == 0 {
		return "[]"
	}
	encoded, _ := json.Marshal(map[string]any{"program": argv[0], "argument_count": len(argv) - 1})
	return string(encoded)
}

func releaseVersions(manifest string) map[string]string {
	value, _ := parseReleaseVersions(manifest)
	return value
}

func parseReleaseVersions(manifest string) (map[string]string, error) {
	var value struct {
		Artifacts map[string]json.RawMessage `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(manifest), &value); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(value.Artifacts))
	for key, raw := range value.Artifacts {
		var exact string
		if err := json.Unmarshal(raw, &exact); err == nil {
			result[key] = strings.TrimPrefix(strings.TrimSpace(exact), "v")
			continue
		}
		var observed []string
		if err := json.Unmarshal(raw, &observed); err != nil {
			return nil, fmt.Errorf("artifact %s has an invalid version", key)
		}
		if len(observed) == 1 {
			result[key] = strings.TrimPrefix(strings.TrimSpace(observed[0]), "v")
		}
	}
	return result, nil
}

func bundleReleaseVersion(versions map[string]string, manifest []byte) string {
	values := make([]string, 0, len(versions))
	for _, value := range versions {
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) > 0 {
		allSame := true
		for _, value := range values[1:] {
			if value != values[0] {
				allSame = false
				break
			}
		}
		if allSame {
			return values[0]
		}
	}
	return "bundle-" + strings.TrimPrefix(stableID("", string(manifest)), "_")[:12]
}
