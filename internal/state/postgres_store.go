package state

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

const (
	postgresDefaultMaxOpenConns = 10
	postgresMaxIdleConns        = 5
	postgresConnMaxLifetime     = 30 * time.Minute
	postgresConnMaxIdleTime     = 5 * time.Minute
	postgresStatementTimeout    = "10s"
	postgresStartupPingTimeout  = 5 * time.Second
)

// PostgresStore is the shared Postgres-backed Store used when N worker
// replicas need one durable state backend. It mirrors SQLiteStore structurally
// (one file per feature) and goes through database/sql via the pure-Go pgx
// stdlib driver, so CGO_ENABLED=0 builds keep working.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStoreFromEnv builds a PostgresStore from OUVRIER_STATE_DSN,
// OUVRIER_STATE_MAX_CONNS, and OUVRIER_STATE_MIGRATE. A missing DSN is a
// startup error naming both configuration variables.
func NewPostgresStoreFromEnv() (*PostgresStore, error) {
	migrateMode, err := migrateModeFromEnv()
	if err != nil {
		return nil, err
	}
	return newPostgresStoreFromEnv(migrateMode)
}

func newPostgresStoreFromEnv(migrateMode string) (*PostgresStore, error) {
	dsn, maxConns, err := postgresConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return newPostgresStore(context.Background(), dsn, maxConns, migrateMode)
}

// postgresConfigFromEnv reads and validates OUVRIER_STATE_DSN and
// OUVRIER_STATE_MAX_CONNS.
func postgresConfigFromEnv() (string, int, error) {
	dsn := strings.TrimSpace(os.Getenv(EnvStateDSN))
	if dsn == "" {
		return "", 0, fmt.Errorf("postgres state store: %s is required when %s=%s", EnvStateDSN, EnvStateBackend, BackendPostgres)
	}
	maxConns := postgresDefaultMaxOpenConns
	if raw := strings.TrimSpace(os.Getenv(EnvStateMaxConns)); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return "", 0, fmt.Errorf("postgres state store: %s must be a positive integer, got %q", EnvStateMaxConns, raw)
		}
		maxConns = parsed
	}
	return dsn, maxConns, nil
}

func NewPostgresStore(dsn string) (*PostgresStore, error) {
	return NewPostgresStoreWithContext(context.Background(), dsn)
}

func NewPostgresStoreWithContext(ctx context.Context, dsn string) (*PostgresStore, error) {
	return newPostgresStore(ctx, dsn, postgresDefaultMaxOpenConns, MigrateAuto)
}

func newPostgresStore(ctx context.Context, dsn string, maxOpenConns int, migrateMode string) (*PostgresStore, error) {
	store, err := openPostgresStore(ctx, dsn, maxOpenConns)
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

// openPostgresStore builds the connection pool and pings it without touching
// the schema, so callers decide whether to migrate or only verify.
func openPostgresStore(ctx context.Context, dsn string, maxOpenConns int) (*PostgresStore, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The parse error is intentionally dropped: pgx may echo the
		// connection string, and the DSN is secret-bearing.
		return nil, fmt.Errorf("postgres state store: invalid DSN (set %s to a valid postgres connection string)", EnvStateDSN)
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	if _, ok := config.RuntimeParams["statement_timeout"]; !ok {
		config.RuntimeParams["statement_timeout"] = postgresStatementTimeout
	}
	if warning := postgresSSLModeWarning(dsn, os.Getenv(envnames.Env)); warning != "" {
		log.Println(warning)
	}

	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(postgresMaxIdleConns)
	db.SetConnMaxLifetime(postgresConnMaxLifetime)
	db.SetConnMaxIdleTime(postgresConnMaxIdleTime)

	store := &PostgresStore{db: db}
	pingCtx, cancel := context.WithTimeout(ctx, postgresStartupPingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("postgres state store: ping: %w", err)
	}
	return store, nil
}

func (s *PostgresStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DBStats exposes connection pool statistics so /admin/health can report on
// the shared backend when OUVRIER_STATE_BACKEND=postgres.
func (s *PostgresStore) DBStats() sql.DBStats {
	return s.db.Stats()
}

// postgresSSLModeWarning returns a non-empty startup warning when the DSN
// disables TLS outside dev. It never includes the DSN itself.
func postgresSSLModeWarning(dsn, ouvrierEnv string) string {
	if !strings.Contains(dsn, "sslmode=disable") {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(ouvrierEnv), "dev") {
		return ""
	}
	return fmt.Sprintf("WARNING: postgres state store: connecting with sslmode=disable while %s != dev; use sslmode=verify-full in production", envnames.Env)
}

// postgresTime normalizes a timestamp written to or read from a TIMESTAMPTZ
// column. Postgres keeps microsecond precision, so round-trips may lose
// sub-microsecond digits; readers compare with a 1µs tolerance where that
// matters.
func postgresTime(t time.Time) time.Time {
	return t.UTC()
}

func nullablePostgresTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func nullablePostgresTimeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time.UTC()
}
