package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

func TestSchemaV5MigratesV4WithBackup(t *testing.T) {
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
	if err != nil || version != 5 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var tables int
	if err := database.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('bundles','bundle_releases','bundle_artifacts','installations','consumer_bindings','bundle_policies','bundle_transactions','bundle_transaction_steps','bundle_receipts','bundle_health_checks','bundle_tasks')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 11 {
		t.Fatalf("bundle tables = %d", tables)
	}
	backups, err := filepath.Glob(path + ".backup-v4-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("migration backups = %v err=%v", backups, err)
	}
}
