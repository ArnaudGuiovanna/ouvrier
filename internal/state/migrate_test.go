package state

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const migrateCLIName = `ouvrier state migrate`

// setSQLiteUserVersion rewrites PRAGMA user_version on an existing database
// file so tests can simulate stale (or too-new) schemas.
func setSQLiteUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

func sqliteUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}

func migratedSQLitePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	return path
}

func TestNewStoreFromEnvRejectsUnknownMigrateMode(t *testing.T) {
	t.Setenv(EnvStateBackend, BackendSQLite)
	t.Setenv(EnvStatePath, filepath.Join(t.TempDir(), "state.db"))
	t.Setenv(EnvStateMigrate, "maybe")

	_, err := NewStoreFromEnv()
	if err == nil {
		t.Fatal("NewStoreFromEnv returned nil error for invalid migrate mode")
	}
	for _, want := range []string{EnvStateMigrate, MigrateAuto, MigrateOff, "maybe"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("invalid-mode error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestNewStoreFromEnvSQLiteMigrateOffRefusesFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	t.Setenv(EnvStateBackend, BackendSQLite)
	t.Setenv(EnvStatePath, path)
	t.Setenv(EnvStateMigrate, MigrateOff)

	_, err := NewStoreFromEnv()
	if err == nil {
		t.Fatal("NewStoreFromEnv returned nil error for fresh database with migrate off")
	}
	if !strings.Contains(err.Error(), migrateCLIName) {
		t.Fatalf("startup-refusal error %q does not name %q", err.Error(), migrateCLIName)
	}
	if got := sqliteUserVersion(t, path); got != 0 {
		t.Fatalf("user_version = %d after refused startup, want 0 (no DDL must run)", got)
	}
}

func TestNewStoreFromEnvSQLiteMigrateOffRefusesStaleSchema(t *testing.T) {
	path := migratedSQLitePath(t)
	setSQLiteUserVersion(t, path, sqliteSchemaVersion-1)

	t.Setenv(EnvStateBackend, BackendSQLite)
	t.Setenv(EnvStatePath, path)
	t.Setenv(EnvStateMigrate, MigrateOff)

	_, err := NewStoreFromEnv()
	if err == nil {
		t.Fatal("NewStoreFromEnv returned nil error for stale schema with migrate off")
	}
	if !strings.Contains(err.Error(), migrateCLIName) {
		t.Fatalf("startup-refusal error %q does not name %q", err.Error(), migrateCLIName)
	}
	if got := sqliteUserVersion(t, path); got != sqliteSchemaVersion-1 {
		t.Fatalf("user_version = %d after refused startup, want %d (no DDL must run)", got, sqliteSchemaVersion-1)
	}
}

func TestNewStoreFromEnvSQLiteMigrateOffRefusesNewerSchema(t *testing.T) {
	path := migratedSQLitePath(t)
	setSQLiteUserVersion(t, path, sqliteSchemaVersion+1)

	t.Setenv(EnvStateBackend, BackendSQLite)
	t.Setenv(EnvStatePath, path)
	t.Setenv(EnvStateMigrate, MigrateOff)

	_, err := NewStoreFromEnv()
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("NewStoreFromEnv error = %v, want newer-schema refusal", err)
	}
}

func TestNewStoreFromEnvSQLiteMigrateOffAcceptsCurrentSchema(t *testing.T) {
	path := migratedSQLitePath(t)
	t.Setenv(EnvStateBackend, BackendSQLite)
	t.Setenv(EnvStatePath, path)
	t.Setenv(EnvStateMigrate, MigrateOff)

	store, err := NewStoreFromEnv()
	if err != nil {
		t.Fatalf("NewStoreFromEnv returned error: %v", err)
	}
	defer closeStore(t, store)

	if err := store.SaveExecution(context.Background(), Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    ExecutionRunning,
		StartedAt: time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
}

func TestMigrateFromEnvSQLiteAppliesOnceThenNoops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	t.Setenv(EnvStateBackend, BackendSQLite)
	t.Setenv(EnvStatePath, path)

	first, err := MigrateFromEnv(context.Background())
	if err != nil {
		t.Fatalf("MigrateFromEnv returned error: %v", err)
	}
	if first.Backend != BackendSQLite {
		t.Fatalf("Backend = %q, want %q", first.Backend, BackendSQLite)
	}
	if len(first.Applied) == 0 || first.Applied[len(first.Applied)-1] != sqliteSchemaVersion {
		t.Fatalf("Applied = %v, want versions up to %d", first.Applied, sqliteSchemaVersion)
	}
	if got := sqliteUserVersion(t, path); got != sqliteSchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, sqliteSchemaVersion)
	}

	second, err := MigrateFromEnv(context.Background())
	if err != nil {
		t.Fatalf("second MigrateFromEnv returned error: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Fatalf("second Applied = %v, want none (idempotent)", second.Applied)
	}
}

func TestSQLiteV8MigrationPreservesLegacyIdempotencyAsSucceeded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	createdAt := formatSQLiteTime(time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC))
	if _, err := db.Exec(`CREATE TABLE ouvrier_idempotency_keys (
		key TEXT PRIMARY KEY,
		exec_id TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy idempotency table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO ouvrier_idempotency_keys (key, exec_id, created_at) VALUES (?, ?, ?)`, "legacy-key", "exec-1", createdAt); err != nil {
		t.Fatalf("insert legacy idempotency row: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatalf("set legacy user version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore migration returned error: %v", err)
	}
	defer store.Close()
	record, ok, err := store.Idempotency(context.Background(), "legacy-key")
	if err != nil || !ok {
		t.Fatalf("Idempotency record=%+v ok=%v err=%v", record, ok, err)
	}
	if record.Outcome != IdempotencySucceeded || record.ExecID != "exec-1" {
		t.Fatalf("migrated record = %+v, want conservative succeeded outcome", record)
	}
	if !record.UpdatedAt.Equal(record.CreatedAt) {
		t.Fatalf("migrated timestamps = %+v, want updated_at backfilled", record)
	}
}

func TestMigrateFromEnvRejectsMemoryBackend(t *testing.T) {
	t.Setenv(EnvStateBackend, BackendMemory)

	_, err := MigrateFromEnv(context.Background())
	if err == nil || !strings.Contains(err.Error(), BackendMemory) {
		t.Fatalf("MigrateFromEnv error = %v, want memory-backend refusal", err)
	}
}

func TestMigrateFromEnvRejectsUnknownBackend(t *testing.T) {
	t.Setenv(EnvStateBackend, "redis")

	_, err := MigrateFromEnv(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unsupported state backend "redis"`) {
		t.Fatalf("MigrateFromEnv error = %v", err)
	}
}

func TestMigrateFromEnvPostgresRequiresDSN(t *testing.T) {
	t.Setenv(EnvStateBackend, BackendPostgres)
	t.Setenv(EnvStateDSN, "")

	_, err := MigrateFromEnv(context.Background())
	if err == nil || !strings.Contains(err.Error(), EnvStateDSN) {
		t.Fatalf("MigrateFromEnv error = %v, want error naming %s", err, EnvStateDSN)
	}
}

func TestNewStoreFromEnvPostgresMigrateOffRefusesPendingMigrations(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	t.Setenv(EnvStateBackend, BackendPostgres)
	t.Setenv(EnvStateDSN, dsn)
	t.Setenv(EnvStateMigrate, MigrateOff)

	_, err := NewStoreFromEnv()
	if err == nil {
		t.Fatal("NewStoreFromEnv returned nil error for pending migrations with migrate off")
	}
	if !strings.Contains(err.Error(), migrateCLIName) {
		t.Fatalf("startup-refusal error %q does not name %q", err.Error(), migrateCLIName)
	}

	// Verification must never create schema objects, not even the
	// migrations table itself.
	db, openErr := sql.Open("pgx", dsn)
	if openErr != nil {
		t.Fatalf("open verification connection: %v", openErr)
	}
	defer db.Close()
	var regclass sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('ouvrier_schema_migrations')::text`).Scan(&regclass); err != nil {
		t.Fatalf("query to_regclass: %v", err)
	}
	if regclass.Valid {
		t.Fatalf("ouvrier_schema_migrations exists after refused startup (= %q); migrate off must never run DDL", regclass.String)
	}
}

func TestNewStoreFromEnvPostgresMigrateOffAcceptsCurrentSchema(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	migrated := openTestPostgresStore(t, dsn)
	if err := migrated.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	t.Setenv(EnvStateBackend, BackendPostgres)
	t.Setenv(EnvStateDSN, dsn)
	t.Setenv(EnvStateMigrate, MigrateOff)

	store, err := NewStoreFromEnv()
	if err != nil {
		t.Fatalf("NewStoreFromEnv returned error: %v", err)
	}
	defer closeStore(t, store)

	if err := store.SaveExecution(context.Background(), Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    ExecutionRunning,
		StartedAt: time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
}

func TestMigrateFromEnvPostgresAppliesOnceThenNoops(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	t.Setenv(EnvStateBackend, BackendPostgres)
	t.Setenv(EnvStateDSN, dsn)

	first, err := MigrateFromEnv(context.Background())
	if err != nil {
		t.Fatalf("MigrateFromEnv returned error: %v", err)
	}
	if first.Backend != BackendPostgres {
		t.Fatalf("Backend = %q, want %q", first.Backend, BackendPostgres)
	}
	if len(first.Applied) != len(postgresMigrations) {
		t.Fatalf("Applied = %v, want all %d migrations", first.Applied, len(postgresMigrations))
	}

	second, err := MigrateFromEnv(context.Background())
	if err != nil {
		t.Fatalf("second MigrateFromEnv returned error: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Fatalf("second Applied = %v, want none (idempotent)", second.Applied)
	}

	// The migrated schema must satisfy a worker starting with migrate off.
	t.Setenv(EnvStateMigrate, MigrateOff)
	store, err := NewStoreFromEnv()
	if err != nil {
		t.Fatalf("NewStoreFromEnv after migrate returned error: %v", err)
	}
	closeStore(t, store)
}

// TestMigrateFromEnvPostgresConcurrentInvocations races two migrate
// invocations against the same schema: the advisory lock must serialize them
// so exactly one applies each migration and neither errors.
func TestMigrateFromEnvPostgresConcurrentInvocations(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	t.Setenv(EnvStateBackend, BackendPostgres)
	t.Setenv(EnvStateDSN, dsn)

	const invocations = 2
	start := make(chan struct{})
	results := make(chan MigrateResult, invocations)
	errs := make(chan error, invocations)
	var wg sync.WaitGroup
	for i := 0; i < invocations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := MigrateFromEnv(context.Background())
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent MigrateFromEnv returned error: %v", err)
	}
	totalApplied := 0
	for result := range results {
		totalApplied += len(result.Applied)
	}
	if totalApplied != len(postgresMigrations) {
		t.Fatalf("total applied across concurrent invocations = %d, want %d (each migration applied exactly once)", totalApplied, len(postgresMigrations))
	}
}
