package state

import (
	"context"
	"fmt"
	"os"
)

// MigrateResult reports what one explicit migration run applied.
type MigrateResult struct {
	// Backend is the resolved state backend (sqlite or postgres).
	Backend string
	// Applied lists the schema versions applied by this run, ascending.
	// Empty means the schema was already current.
	Applied []int
}

// MigrateFromEnv connects to the state backend configured by
// OUVRIER_STATE_BACKEND (and OUVRIER_STATE_PATH / OUVRIER_STATE_DSN), applies
// pending schema migrations, and reports the versions it applied. It backs
// `ouvrier state migrate` and deliberately ignores OUVRIER_STATE_MIGRATE:
// invoking it is the explicit request to run DDL. Postgres migrations run in
// one transaction under pg_advisory_xact_lock, so concurrent invocations are
// safe; SQLite applies its idempotent user_version steps.
func MigrateFromEnv(ctx context.Context) (MigrateResult, error) {
	switch backend := backendFromEnv(); backend {
	case BackendSQLite:
		store, err := openSQLiteStore(ctx, os.Getenv(EnvStatePath))
		if err != nil {
			return MigrateResult{}, err
		}
		defer func() {
			_ = store.Close()
		}()
		applied, err := store.migrate(ctx)
		if err != nil {
			return MigrateResult{}, err
		}
		return MigrateResult{Backend: BackendSQLite, Applied: applied}, nil
	case BackendPostgres:
		dsn, maxConns, err := postgresConfigFromEnv()
		if err != nil {
			return MigrateResult{}, err
		}
		store, err := openPostgresStore(ctx, dsn, maxConns)
		if err != nil {
			return MigrateResult{}, err
		}
		defer func() {
			_ = store.Close()
		}()
		applied, err := store.migrate(ctx)
		if err != nil {
			return MigrateResult{}, err
		}
		return MigrateResult{Backend: BackendPostgres, Applied: applied}, nil
	case BackendMemory:
		return MigrateResult{}, fmt.Errorf("state backend %q keeps no schema to migrate; set %s to %s or %s", BackendMemory, EnvStateBackend, BackendSQLite, BackendPostgres)
	default:
		return MigrateResult{}, fmt.Errorf("unsupported state backend %q", backend)
	}
}
