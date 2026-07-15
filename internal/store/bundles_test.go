package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
)

func TestSchemaV6MigratesV4WithBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := open(path, "rwc", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version, ok := migrationVersion(entry.Name())
		if !ok || version > 4 {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := OpenRW(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	version, err := database.UserVersion(context.Background())
	if err != nil || version != 6 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var tables int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('bundles','bundle_releases','bundle_artifacts','installations','consumer_bindings','bundle_policies','bundle_transactions','bundle_transaction_steps','bundle_receipts','bundle_health_checks','bundle_tasks','reconcile_runs')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 12 {
		t.Fatalf("bundle tables = %d", tables)
	}
	backups, err := filepath.Glob(path + ".backup-v4-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("migration backups = %v err=%v", backups, err)
	}
}

func TestConfigureBundleMarksPhysicalInstallationsManaged(t *testing.T) {
	database, err := OpenRW(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	bundle := model.Bundle{ID: "bundle", Slug: "bundle", Name: "Bundle", RecipeID: "bundle", RecipeVersion: "1", RecipeSource: "builtin", Owner: model.LifecycleDelegated, ConfigState: model.BundleUnconfigured, Confidence: model.BundleConfidenceHigh, DiscoveredAt: now, LastSeenAt: now}
	if err := database.UpsertBundle(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	installation := model.Installation{ID: "installation", BundleID: bundle.ID, Driver: "git-skill", Path: "/tmp/skill", Owner: model.LifecycleDelegated, LastSeenAt: now}
	if err := database.UpsertInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := database.ConfigureBundle(ctx, model.BundlePolicy{BundleID: bundle.ID, Mode: model.BundlePolicyAuto, RecipeTrusted: true, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	installations, err := database.ListInstallations(ctx, bundle.ID)
	if err != nil || len(installations) != 1 || !installations[0].Managed {
		t.Fatalf("installations = %#v, err = %v", installations, err)
	}
	if err := database.ConfigureBundle(ctx, model.BundlePolicy{BundleID: bundle.ID, Mode: model.BundlePolicyObserve, RecipeTrusted: true, UpdatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	installations, err = database.ListInstallations(ctx, bundle.ID)
	if err != nil || installations[0].Managed {
		t.Fatalf("observed installations = %#v, err = %v", installations, err)
	}
}
