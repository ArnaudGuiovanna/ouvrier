package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultSQLitePath   = ".ouvrier/state.db"
	sqliteSchemaVersion = 3
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	return NewSQLiteStoreWithContext(context.Background(), path)
}

func NewSQLiteStoreWithContext(ctx context.Context, path string) (*SQLiteStore, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		path = DefaultSQLitePath
	}
	if err := ensureSQLiteParent(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite state store: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &SQLiteStore{db: db}
	if err := store.configure(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) configure(ctx context.Context) error {
	for _, stmt := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("configure sqlite state store: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	for _, stmt := range sqliteSchemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate sqlite state store: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "ouvrier_sessions", "max_wallclock_ns", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate sqlite state store: %w", err)
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", sqliteSchemaVersion))
	if err != nil {
		return fmt.Errorf("set sqlite state schema version: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

func ensureSQLiteParent(path string) error {
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create sqlite state directory: %w", err)
	}
	return nil
}

func activeContext(ctx context.Context) (context.Context, error) {
	if ctx == nil {
		return context.Background(), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ctx, nil
}

func formatSQLiteTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseSQLiteTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func nullableSQLiteTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatSQLiteTime(t)
}

func parseNullableSQLiteTime(value sql.NullString) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, nil
	}
	return parseSQLiteTime(value.String)
}

func missingSQLiteRow(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

var sqliteSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS ouvrier_executions (
		exec_id TEXT PRIMARY KEY,
		trace_id TEXT NOT NULL,
		status TEXT NOT NULL,
		started_at TEXT NOT NULL,
		completed_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_sessions (
		session_id TEXT PRIMARY KEY,
		exec_id TEXT NOT NULL,
		parent_session_id TEXT NOT NULL,
		trace_id TEXT NOT NULL,
		model TEXT NOT NULL,
		started_at TEXT NOT NULL,
		max_iterations INTEGER NOT NULL,
		max_tokens INTEGER NOT NULL,
		max_cost_usd REAL NOT NULL,
		max_wallclock_ns INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_idempotency_keys (
		key TEXT PRIMARY KEY,
		exec_id TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		kind TEXT NOT NULL,
		exec_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		trace_id TEXT NOT NULL,
		payload TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ouvrier_events_exec_id ON ouvrier_events(exec_id, id)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_schema_violations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		exec_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		schema_name TEXT NOT NULL,
		error TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_memory (
		scope TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (scope, key)
	)`,
}
