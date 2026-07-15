package bundle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type RecoveryResult struct {
	FailedBeforeActivation int `json:"failed_before_activation"`
	CompensatedUpdates     int `json:"compensated_updates"`
	CompletedRollbacks     int `json:"completed_rollbacks"`
}

func (r RecoveryResult) Total() int {
	return r.FailedBeforeActivation + r.CompensatedUpdates + r.CompletedRollbacks
}

// RecoverTransactions makes every unfinished Bundle journal terminal before a
// new inventory scan or update can start. Ambiguous in-flight commands are
// deliberately re-run toward a known exact version; eligible recipes must make
// their activation and rollback commands idempotent.
func (s Service) RecoverTransactions(ctx context.Context) (RecoveryResult, error) {
	if err := s.validate(); err != nil {
		return RecoveryResult{}, err
	}
	transactions, err := s.Database.ListUnfinishedBundleTransactions(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	var result RecoveryResult
	for _, transaction := range transactions {
		switch transaction.Status {
		case model.BundleTransactionPrepared, model.BundleTransactionStaging:
			completed := s.now()
			if err := s.Database.UpdateBundleTransaction(ctx, transaction.ID, model.BundleTransactionFailed, "recovered_before_activation", "", &completed); err != nil {
				return result, err
			}
			_ = os.RemoveAll(filepath.Join(s.Paths.StagingDir, transaction.ID))
			result.FailedBeforeActivation++
		case model.BundleTransactionActivating:
			steps, _, err := s.loadRecoverySteps(ctx, transaction)
			if err != nil {
				return result, err
			}
			var activated []executionStep
			for _, step := range steps {
				switch step.record.Status {
				case model.BundleStepActivating, model.BundleStepActivated, model.BundleStepHealthy, model.BundleStepFailed, model.BundleStepCompensating:
					activated = append(activated, step)
				}
			}
			if err := s.compensate(ctx, transaction, activated); err != nil {
				return result, fmt.Errorf("recover bundle transaction %s: %w", transaction.ID, err)
			}
			if err := s.putRecoveredRollbackReceipt(ctx, transaction); err != nil {
				return result, err
			}
			_ = os.RemoveAll(filepath.Join(s.Paths.StagingDir, transaction.ID))
			result.CompensatedUpdates++
		case model.BundleTransactionRollingBack:
			steps, _, err := s.loadRecoverySteps(ctx, transaction)
			if err != nil {
				return result, err
			}
			explicit := false
			for _, step := range steps {
				explicit = explicit || step.record.Kind == "rollback"
			}
			if explicit {
				fromVersions, err := s.releaseVersionsByID(ctx, transaction.FromReleaseID)
				if err != nil {
					return result, err
				}
				if err := s.finishExplicitRollback(ctx, transaction, steps, fromVersions); err != nil {
					return result, err
				}
				result.CompletedRollbacks++
			} else {
				var activated []executionStep
				for _, step := range steps {
					switch step.record.Status {
					case model.BundleStepActivating, model.BundleStepActivated, model.BundleStepHealthy, model.BundleStepFailed, model.BundleStepCompensating:
						activated = append(activated, step)
					}
				}
				if err := s.compensate(ctx, transaction, activated); err != nil {
					return result, fmt.Errorf("recover bundle rollback %s: %w", transaction.ID, err)
				}
				if err := s.putRecoveredRollbackReceipt(ctx, transaction); err != nil {
					return result, err
				}
				result.CompensatedUpdates++
			}
			_ = os.RemoveAll(filepath.Join(s.Paths.StagingDir, transaction.ID))
		}
	}
	return result, nil
}

func (s Service) releaseVersionsByID(ctx context.Context, releaseID string) (map[string]string, error) {
	if releaseID == "" {
		return map[string]string{}, nil
	}
	release, err := s.Database.GetBundleRelease(ctx, releaseID)
	if err != nil {
		return nil, err
	}
	return parseReleaseVersions(release.ManifestJSON)
}

func (s Service) loadRecoverySteps(ctx context.Context, transaction model.BundleTransaction) ([]executionStep, map[string]string, error) {
	records, err := s.Database.ListBundleTransactionSteps(ctx, transaction.ID)
	if err != nil {
		return nil, nil, err
	}
	artifacts, err := s.Database.ListBundleArtifacts(ctx, transaction.BundleID)
	if err != nil {
		return nil, nil, err
	}
	installations, err := s.Database.ListInstallations(ctx, transaction.BundleID)
	if err != nil {
		return nil, nil, err
	}
	artifactByID := make(map[string]model.BundleArtifact, len(artifacts))
	for _, artifact := range artifacts {
		artifactByID[artifact.ID] = artifact
	}
	installationByID := make(map[string]model.Installation, len(installations))
	for _, installation := range installations {
		installationByID[installation.ID] = installation
	}
	versions := map[string]string{}
	if transaction.ToReleaseID != "" {
		release, err := s.Database.GetBundleRelease(ctx, transaction.ToReleaseID)
		if err != nil {
			return nil, nil, err
		}
		versions, err = parseReleaseVersions(release.ManifestJSON)
		if err != nil {
			return nil, nil, err
		}
	}
	steps := make([]executionStep, 0, len(records))
	for _, record := range records {
		artifact, ok := artifactByID[record.ArtifactID]
		if !ok {
			return nil, nil, fmt.Errorf("recover bundle transaction %s: artifact %s is missing", transaction.ID, record.ArtifactID)
		}
		installation, ok := installationByID[record.InstallationID]
		if !ok {
			return nil, nil, fmt.Errorf("recover bundle transaction %s: installation %s is missing", transaction.ID, record.InstallationID)
		}
		recipe, err := decodeArtifactMetadata(artifact)
		if err != nil {
			return nil, nil, err
		}
		rollbackVersion := installation.ObservedVersion
		if record.Kind == "rollback" {
			rollbackVersion = versions[artifact.RecipeKey]
		}
		steps = append(steps, executionStep{record: record, artifact: artifact, installation: installation, recipe: recipe,
			version: versions[artifact.RecipeKey], rollbackVersion: rollbackVersion,
			stagePath: filepath.Join(s.Paths.StagingDir, transaction.ID, fmt.Sprintf("%03d", record.Ordinal))})
	}
	return steps, versions, nil
}

func (s Service) putRecoveredRollbackReceipt(ctx context.Context, transaction model.BundleTransaction) error {
	completed := s.now()
	receiptID, _ := model.NewID("brc")
	receipt := model.BundleReceipt{ID: receiptID, BundleID: transaction.BundleID, TransactionID: transaction.ID,
		ReleaseID: transaction.ToReleaseID, Action: "update", Status: "rolled_back", SummaryJSON: "{}", CreatedAt: completed}
	return s.Database.CommitBundleCompensation(ctx, transaction.ID, "recovered_interrupted_update", receipt, completed)
}

func (s Service) finishExplicitRollback(ctx context.Context, transaction model.BundleTransaction, steps []executionStep, fromVersions map[string]string) error {
	completedSteps := make([]executionStep, 0, len(steps))
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		if step.record.Status == model.BundleStepCompensated || step.record.Status == model.BundleStepHealthy {
			completedSteps = append(completedSteps, step)
			continue
		}
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepCompensating, "", "", "{}", nil); err != nil {
			return err
		}
		if err := s.runCommand(ctx, step.recipe.RollbackArgv, step, DefaultInstallTimeout); err != nil {
			completedSteps = append(completedSteps, step)
			restoreErr := s.restoreAfterRollbackFailure(ctx, completedSteps, fromVersions)
			return s.failTransaction(ctx, transaction, "recovery_rollback_failed", errors.Join(err, restoreErr))
		}
		completed := s.now()
		if err := s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepCompensated, "", "", "{}", &completed); err != nil {
			return err
		}
		completedSteps = append(completedSteps, step)
	}
	for index := len(steps) - 1; index >= 0; index-- {
		step := steps[index]
		if err := s.runCommand(ctx, step.recipe.HealthArgv, step, DefaultHealthTimeout); err != nil {
			restoreErr := s.restoreAfterRollbackFailure(ctx, completedSteps, fromVersions)
			return s.failTransaction(ctx, transaction, "recovery_rollback_health_failed", errors.Join(err, restoreErr))
		}
		completed := s.now()
		_ = s.Database.UpdateBundleTransactionStep(ctx, step.record.ID, model.BundleStepHealthy, "", "", "{}", &completed)
	}
	completed := s.now()
	observations := make([]store.InstallationObservation, 0, len(steps))
	for _, step := range steps {
		observations = append(observations, store.InstallationObservation{InstallationID: step.installation.ID, Version: step.version})
	}
	receiptID, _ := model.NewID("brc")
	receipt := model.BundleReceipt{ID: receiptID, BundleID: transaction.BundleID, TransactionID: transaction.ID,
		ReleaseID: transaction.ToReleaseID, Action: "rollback", Status: "succeeded", SummaryJSON: "{}", CreatedAt: completed}
	if err := s.Database.CommitBundleActivation(ctx, transaction.ID, transaction.BundleID, transaction.FromReleaseID, transaction.ToReleaseID, observations, receipt, completed); err != nil {
		restoreErr := s.restoreAfterRollbackFailure(ctx, completedSteps, fromVersions)
		return s.failTransaction(ctx, transaction, "recovery_rollback_commit_failed", errors.Join(err, restoreErr))
	}
	return nil
}
