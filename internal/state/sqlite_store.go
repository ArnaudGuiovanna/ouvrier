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
	sqliteSchemaVersion = 6
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	return NewSQLiteStoreWithContext(context.Background(), path)
}

func NewSQLiteStoreWithContext(ctx context.Context, path string) (*SQLiteStore, error) {
	return newSQLiteStore(ctx, path, MigrateAuto)
}

func newSQLiteStore(ctx context.Context, path string, migrateMode string) (*SQLiteStore, error) {
	store, err := openSQLiteStore(ctx, path)
	if err != nil {
		return nil, err
	}
	if migrateMode == MigrateOff {
		if err := store.verifySchema(ctx); err != nil {
			_ = store.Close()
			return nil, err
		}
		return store, nil
	}
	if _, err := store.migrate(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// openSQLiteStore opens and configures the database without touching the
// schema, so callers decide whether to migrate or only verify.
func openSQLiteStore(ctx context.Context, path string) (*SQLiteStore, error) {
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

// migrate applies the idempotent schema statements and stamps PRAGMA
// user_version. It returns the schema versions newly crossed by this run
// (empty when the database was already current), so `ouvrier state migrate`
// can report what it did.
func (s *SQLiteStore) migrate(ctx context.Context) ([]int, error) {
	before, err := s.schemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	for _, stmt := range sqliteSchemaStatements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("migrate sqlite state store: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "ouvrier_sessions", "max_wallclock_ns", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, fmt.Errorf("migrate sqlite state store: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", sqliteSchemaVersion)); err != nil {
		return nil, fmt.Errorf("set sqlite state schema version: %w", err)
	}
	var applied []int
	for version := before + 1; version <= sqliteSchemaVersion; version++ {
		applied = append(applied, version)
	}
	return applied, nil
}

// verifySchema checks PRAGMA user_version against the version this binary
// expects, without ever running DDL. It backs OUVRIER_STATE_MIGRATE=off.
func (s *SQLiteStore) verifySchema(ctx context.Context) error {
	version, err := s.schemaVersion(ctx)
	if err != nil {
		return err
	}
	switch {
	case version < sqliteSchemaVersion:
		return fmt.Errorf("sqlite state store: schema version %d, want %d; run \"ouvrier state migrate\" to apply pending migrations (%s=%s never runs DDL)",
			version, sqliteSchemaVersion, EnvStateMigrate, MigrateOff)
	case version > sqliteSchemaVersion:
		return fmt.Errorf("sqlite state store: schema version %d is newer than this binary supports (%d); upgrade the worker",
			version, sqliteSchemaVersion)
	}
	return nil
}

func (s *SQLiteStore) schemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read sqlite state schema version: %w", err)
	}
	return version, nil
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
	`CREATE TABLE IF NOT EXISTS ouvrier_approvals (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL UNIQUE,
		exec_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		trace_id TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		tool_call_id TEXT NOT NULL,
		tool_kind TEXT NOT NULL,
		effect TEXT NOT NULL,
		reason TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		decided_at TEXT,
		decided_by TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ouvrier_approvals_status ON ouvrier_approvals(status, seq)`,
	// Schema v5: fenced TTL leases for cron leader election and durable-run
	// recovery claims. Timestamps are SQLite-generated strftime strings with
	// fixed millisecond width so expiry comparisons stay in database time.
	`CREATE TABLE IF NOT EXISTS ouvrier_leases (
		name TEXT PRIMARY KEY,
		holder TEXT NOT NULL,
		fence INTEGER NOT NULL,
		acquired_at TEXT NOT NULL,
		renewed_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`,
	// Schema v6: durable runs — step-checkpoint journal and tool intents
	// (OUVRIER_DURABLE_RUNS=1). Journal input and checkpoint output are
	// redacted before they reach disk; tool intents are metadata only.
	`CREATE TABLE IF NOT EXISTS ouvrier_run_journal (
		exec_id TEXT PRIMARY KEY,
		plan_key TEXT NOT NULL,
		plan_hash TEXT NOT NULL,
		trigger_kind TEXT NOT NULL,
		input TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ouvrier_run_journal_created_at ON ouvrier_run_journal(created_at)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_run_checkpoints (
		exec_id TEXT NOT NULL,
		step_index INTEGER NOT NULL,
		output TEXT NOT NULL,
		completed_at TEXT NOT NULL,
		PRIMARY KEY (exec_id, step_index)
	)`,
	`CREATE TABLE IF NOT EXISTS ouvrier_tool_intents (
		exec_id TEXT NOT NULL,
		tool_call_id TEXT NOT NULL,
		step_index INTEGER NOT NULL,
		tool_name TEXT NOT NULL,
		effect TEXT NOT NULL,
		idem_key TEXT NOT NULL,
		started_at TEXT NOT NULL,
		completed_at TEXT,
		PRIMARY KEY (exec_id, tool_call_id)
	)`,
}
