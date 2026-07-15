package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/store"
)

type transactionRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *transactionRunner) Run(_ context.Context, name string, args ...string) (execx.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
	switch name {
	case "resolver":
		return execx.Result{Stdout: []byte("2.0.0\n")}, nil
	case "activate-two":
		return execx.Result{}, errors.New("activation failed")
	default:
		return execx.Result{}, nil
	}
}

func TestBundleTransactionStagesAllArtifactsBeforeActivationAndCompensates(t *testing.T) {
	root := t.TempDir()
	paths := config.ResolveWith(root, func(key string) string {
		if key == config.EnvHome {
			return filepath.Join(root, "tooltend")
		}
		return ""
	})
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	bundleValue := model.Bundle{ID: "bundle", Slug: "bundle", Name: "Bundle", RecipeID: "test", RecipeVersion: "1", RecipeSource: "local", Owner: model.LifecycleDelegated, ConfigState: model.BundleUnconfigured, Confidence: model.BundleConfidenceHigh, MetadataJSON: "{}", DiscoveredAt: now, LastSeenAt: now}
	if err := database.UpsertBundle(context.Background(), bundleValue); err != nil {
		t.Fatal(err)
	}
	makeArtifact := func(id, key, activate string, ordinal int) model.BundleArtifact {
		recipe := ArtifactRecipe{Key: key, Name: key, Kind: model.ArtifactCLI, Driver: "test", Required: true,
			ResolveArgv: []string{"resolver"}, StageArgv: []string{"stage-" + key, "${stage}"}, ActivateArgv: []string{activate, "${path}"}, RollbackArgv: []string{"rollback-" + key, "${previous_version}"}, HealthArgv: []string{"health-" + key}}
		metadata, _ := jsonMarshal(recipe)
		return model.BundleArtifact{ID: id, BundleID: bundleValue.ID, RecipeKey: key, Kind: model.ArtifactCLI, Name: key, Ordinal: ordinal, Required: true, Driver: "test", MetadataJSON: metadata}
	}
	artifacts := []model.BundleArtifact{makeArtifact("artifact-one", "one", "activate-one", 0), makeArtifact("artifact-two", "two", "activate-two", 1)}
	for _, artifact := range artifacts {
		if err := database.UpsertBundleArtifact(context.Background(), artifact); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, artifact.RecipeKey)
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := database.UpsertInstallation(context.Background(), model.Installation{ID: "installation-" + artifact.RecipeKey, BundleID: bundleValue.ID, ArtifactID: artifact.ID, Driver: "test", Path: path, PackageIdentity: artifact.RecipeKey, ObservedVersion: "1.0.0", Owner: model.LifecycleDelegated, MetadataJSON: "{}", LastSeenAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	policy := model.BundlePolicy{BundleID: bundleValue.ID, Mode: model.BundlePolicyManual, RecipeTrusted: true, UpdatedAt: now}
	if err := database.ConfigureBundle(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	current, err := database.GetBundle(context.Background(), bundleValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &transactionRunner{}
	service := Service{Database: database, Paths: paths, Runner: runner}
	preview, err := service.PrepareUpdate(context.Background(), current.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteUpdate(context.Background(), preview); err == nil {
		t.Fatal("expected activation failure")
	}
	runner.mu.Lock()
	calls := append([]string(nil), runner.calls...)
	runner.mu.Unlock()
	wantPrefix := []string{"resolver", "resolver", "stage-one", "stage-two", "activate-one"}
	if len(calls) < len(wantPrefix)+4 {
		t.Fatalf("calls = %#v", calls)
	}
	for index, want := range wantPrefix {
		if calls[index] != want {
			t.Fatalf("calls = %#v", calls)
		}
	}
	if calls[len(calls)-1] != "rollback-one" {
		t.Fatalf("calls = %#v", calls)
	}
	var status string
	if err := database.DB().QueryRow(`SELECT status FROM bundle_transactions ORDER BY started_at DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(model.BundleTransactionRolledBack) {
		t.Fatalf("transaction status = %s", status)
	}
}

func TestBundleRollbackUsesTargetReleaseAndUpdatesPhysicalObservation(t *testing.T) {
	root := t.TempDir()
	paths := config.ResolveWith(root, func(key string) string {
		if key == config.EnvHome {
			return filepath.Join(root, "tooltend")
		}
		return ""
	})
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	bundleValue := model.Bundle{ID: "bundle", Slug: "bundle", Name: "Bundle", RecipeID: "test", RecipeVersion: "1", RecipeSource: "local",
		Owner: model.LifecycleDelegated, ConfigState: model.BundleUnconfigured, Confidence: model.BundleConfidenceHigh, MetadataJSON: "{}", DiscoveredAt: now, LastSeenAt: now}
	if err := database.UpsertBundle(ctx, bundleValue); err != nil {
		t.Fatal(err)
	}
	recipe := ArtifactRecipe{Key: "cli", Name: "CLI", Kind: model.ArtifactCLI, Driver: "test", Required: true,
		ResolveArgv: []string{"resolver"}, StageArgv: []string{"stage", "${stage}"}, ActivateArgv: []string{"activate", "${version}"},
		RollbackArgv: []string{"rollback", "${version}"}, HealthArgv: []string{"health"}}
	metadata, _ := jsonMarshal(recipe)
	artifact := model.BundleArtifact{ID: "artifact", BundleID: bundleValue.ID, RecipeKey: "cli", Kind: model.ArtifactCLI, Name: "CLI", Required: true, Driver: "test", MetadataJSON: metadata}
	if err := database.UpsertBundleArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	installation := model.Installation{ID: "installation", BundleID: bundleValue.ID, ArtifactID: artifact.ID, Driver: "test", Path: filepath.Join(root, "cli"),
		PackageIdentity: "cli", ObservedVersion: "2.0.0", Owner: model.LifecycleDelegated, MetadataJSON: "{}", LastSeenAt: now}
	if err := database.UpsertInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	oldRelease := model.BundleRelease{ID: "release-old", BundleID: bundleValue.ID, Version: "1.0.0", ManifestJSON: `{"artifacts":{"cli":"1.0.0"}}`, Status: "active", CreatedAt: now.Add(-time.Hour)}
	currentRelease := model.BundleRelease{ID: "release-current", BundleID: bundleValue.ID, Version: "2.0.0", ManifestJSON: `{"artifacts":{"cli":"2.0.0"}}`, Status: "active", CreatedAt: now}
	for _, release := range []model.BundleRelease{oldRelease, currentRelease} {
		if err := database.UpsertBundleRelease(ctx, release); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SetBundleCurrentRelease(ctx, bundleValue.ID, currentRelease.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureBundle(ctx, model.BundlePolicy{BundleID: bundleValue.ID, Mode: model.BundlePolicyManual, RecipeTrusted: true, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	configured, err := database.GetBundle(ctx, bundleValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := &transactionRunner{}
	service := Service{Database: database, Paths: paths, Runner: runner}
	preview, err := service.PrepareRollback(ctx, configured.ID, oldRelease.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ExecuteRollback(ctx, preview)
	if err != nil {
		t.Fatal(err)
	}
	if result.Release.ID != oldRelease.ID || result.Receipt.Action != "rollback" || result.Receipt.Status != "succeeded" {
		t.Fatalf("rollback result = %#v", result)
	}
	updated, err := database.GetBundle(ctx, bundleValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentReleaseID != oldRelease.ID {
		t.Fatalf("current release = %s", updated.CurrentReleaseID)
	}
	installations, err := database.ListInstallations(ctx, bundleValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(installations) != 1 || installations[0].ObservedVersion != "1.0.0" {
		t.Fatalf("installations = %#v", installations)
	}
}

func TestRecoverTransactionsCompensatesAmbiguousActivation(t *testing.T) {
	root := t.TempDir()
	paths := config.ResolveWith(root, func(key string) string {
		if key == config.EnvHome {
			return filepath.Join(root, "tooltend")
		}
		return ""
	})
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenRW(paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	bundleValue := model.Bundle{ID: "bundle", Slug: "bundle", Name: "Bundle", RecipeID: "test", RecipeVersion: "1", RecipeSource: "local",
		Owner: model.LifecycleDelegated, ConfigState: model.BundleConfigured, Confidence: model.BundleConfidenceHigh, MetadataJSON: "{}", DiscoveredAt: now, LastSeenAt: now}
	if err := database.UpsertBundle(ctx, bundleValue); err != nil {
		t.Fatal(err)
	}
	recipe := ArtifactRecipe{Key: "cli", Name: "CLI", Kind: model.ArtifactCLI, Driver: "test", Required: true,
		RollbackArgv: []string{"rollback", "${rollback_version}"}, HealthArgv: []string{"health"}}
	metadata, _ := jsonMarshal(recipe)
	artifact := model.BundleArtifact{ID: "artifact", BundleID: bundleValue.ID, RecipeKey: "cli", Kind: model.ArtifactCLI, Name: "CLI", Required: true, Driver: "test", MetadataJSON: metadata}
	if err := database.UpsertBundleArtifact(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	installation := model.Installation{ID: "installation", BundleID: bundleValue.ID, ArtifactID: artifact.ID, Driver: "test", Path: filepath.Join(root, "cli"),
		PackageIdentity: "cli", ObservedVersion: "1.0.0", Owner: model.LifecycleDelegated, MetadataJSON: "{}", LastSeenAt: now}
	if err := database.UpsertInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	target := model.BundleRelease{ID: "release-target", BundleID: bundleValue.ID, Version: "2.0.0", ManifestJSON: `{"artifacts":{"cli":"2.0.0"}}`, Status: "resolved", CreatedAt: now}
	if err := database.UpsertBundleRelease(ctx, target); err != nil {
		t.Fatal(err)
	}
	transaction := model.BundleTransaction{ID: "transaction", BundleID: bundleValue.ID, ToReleaseID: target.ID, Status: model.BundleTransactionActivating, StartedAt: now, UpdatedAt: now}
	if err := database.PutBundleTransaction(ctx, transaction); err != nil {
		t.Fatal(err)
	}
	step := model.BundleTransactionStep{ID: "step", TransactionID: transaction.ID, ArtifactID: artifact.ID, InstallationID: installation.ID,
		Kind: "cli", Status: model.BundleStepActivating, CommandJSON: "{}", RollbackJSON: "{}", BeforeJSON: "{}", AfterJSON: "{}"}
	if err := database.PutBundleTransactionStep(ctx, step); err != nil {
		t.Fatal(err)
	}
	stagePath := filepath.Join(paths.StagingDir, transaction.ID)
	if err := os.MkdirAll(stagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &transactionRunner{}
	service := Service{Database: database, Paths: paths, Runner: runner}
	result, err := service.RecoverTransactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.CompensatedUpdates != 1 || result.Total() != 1 {
		t.Fatalf("recovery result = %+v", result)
	}
	var status string
	if err := database.DB().QueryRow(`SELECT status FROM bundle_transactions WHERE id=?`, transaction.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(model.BundleTransactionRolledBack) {
		t.Fatalf("transaction status = %s", status)
	}
	if _, err := os.Stat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery staging path still exists: %v", err)
	}
	runner.mu.Lock()
	calls := append([]string(nil), runner.calls...)
	runner.mu.Unlock()
	if len(calls) != 1 || calls[0] != "rollback" {
		t.Fatalf("recovery calls = %#v", calls)
	}
}

func jsonMarshal(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}
