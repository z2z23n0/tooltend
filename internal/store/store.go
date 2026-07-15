package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 5

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	db   *sql.DB
	path string
}

func OpenRW(path string) (*Store, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("store: database path must be absolute")
	}
	existed := false
	if info, err := os.Stat(path); err == nil {
		existed = info.Size() > 0
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create state directory: %w", err)
	}
	db, err := open(path, "rwc", 5000, true)
	if err != nil {
		return nil, err
	}
	result := &Store{db: db, path: path}
	if err := result.migrate(existed); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: chmod database: %w", err)
	}
	return result, nil
}

// OpenHook opens an existing migrated database with a single connection and no busy wait.
// It never creates files or runs migrations.
func OpenHook(path string) (*Store, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("store: hook database unavailable: %w", err)
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("store: hook database is not a regular file")
	}
	db, err := open(path, "rw", 0, false)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	result := &Store{db: db, path: path}
	version, err := result.UserVersion(context.Background())
	if err != nil || version != SchemaVersion {
		_ = db.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store: hook database schema %d, expected %d", version, SchemaVersion)
	}
	return result, nil
}

func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := open(path, "ro", 0, false)
	if err != nil {
		return nil, err
	}
	result := &Store{db: db, path: path}
	return result, nil
}

func open(path, mode string, busyMS int, wal bool) (*sql.DB, error) {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Set("mode", mode)
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout("+strconv.Itoa(busyMS)+")")
	if wal {
		query.Add("_pragma", "journal_mode(WAL)")
		query.Add("_pragma", "synchronous(NORMAL)")
	}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}
	return db, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }

func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

func (s *Store) UserVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return version, nil
}

func (s *Store) migrate(existed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	current, err := s.UserVersion(ctx)
	if err != nil {
		return err
	}
	if current > SchemaVersion {
		return fmt.Errorf("store: database schema %d is newer than supported %d", current, SchemaVersion)
	}
	if current == SchemaVersion {
		return s.integrityCheck(ctx)
	}
	if existed {
		backupPath := fmt.Sprintf("%s.backup-v%d-%s", s.path, current, time.Now().UTC().Format("20060102T150405.000000000Z"))
		if err := vacuumInto(ctx, s.db, backupPath); err != nil {
			return fmt.Errorf("store: backup before migration: %w", err)
		}
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version, ok := migrationVersion(entry.Name())
		if !ok || version <= current {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		err = s.WithTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("store: apply migration %s: %w", entry.Name(), err)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO schema_migrations(version, applied_at) VALUES (?, ?)", version, now); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version))
			return err
		})
		if err != nil {
			return err
		}
		current = version
	}
	if current != SchemaVersion {
		return fmt.Errorf("store: migrations ended at %d, expected %d", current, SchemaVersion)
	}
	return s.integrityCheck(ctx)
}

func migrationVersion(name string) (int, bool) {
	part, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, false
	}
	value, err := strconv.Atoi(part)
	return value, err == nil
}

func vacuumInto(ctx context.Context, db *sql.DB, destination string) error {
	if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
		return err
	}
	escaped := strings.ReplaceAll(destination, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return err
	}
	return os.Chmod(destination, 0o600)
}

func (s *Store) integrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("store: integrity check: %s", result)
	}
	return nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func timeText(value time.Time) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func nullableTimeText(value *time.Time) any {
	if value == nil {
		return nil
	}
	return timeText(*value)
}
func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }

func scanNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
