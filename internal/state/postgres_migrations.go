package state

import (
	"context"
	"fmt"
)

// postgresMigration is one ordered, additive schema step. Applied versions are
// recorded in ouvrier_schema_migrations and never re-run.
type postgresMigration struct {
	version    int
	statements []string
}

// postgresMigrations is the ordered Postgres schema history. v0.3.x
// migrations stay additive-only.
var postgresMigrations = []postgresMigration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE ouvrier_executions (
				exec_id TEXT PRIMARY KEY,
				trace_id TEXT NOT NULL,
				status TEXT NOT NULL,
				started_at TIMESTAMPTZ NOT NULL,
				completed_at TIMESTAMPTZ
			)`,
			`CREATE TABLE ouvrier_sessions (
				session_id TEXT PRIMARY KEY,
				exec_id TEXT NOT NULL,
				parent_session_id TEXT NOT NULL,
				trace_id TEXT NOT NULL,
				model TEXT NOT NULL,
				started_at TIMESTAMPTZ NOT NULL,
				max_iterations BIGINT NOT NULL,
				max_tokens BIGINT NOT NULL,
				max_cost_usd DOUBLE PRECISION NOT NULL,
				max_wallclock_ns BIGINT NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE ouvrier_idempotency_keys (
				key TEXT PRIMARY KEY,
				exec_id TEXT NOT NULL,
				created_at TIMESTAMPTZ NOT NULL
			)`,
			// Event payloads stay TEXT (not JSONB): persisted JSON must
			// round-trip byte-for-byte.
			`CREATE TABLE ouvrier_events (
				id BIGINT PRIMARY KEY,
				at TIMESTAMPTZ NOT NULL,
				kind TEXT NOT NULL,
				exec_id TEXT NOT NULL,
				session_id TEXT NOT NULL,
				trace_id TEXT NOT NULL,
				payload TEXT NOT NULL
			)`,
			`CREATE INDEX idx_ouvrier_events_exec_id ON ouvrier_events(exec_id, id)`,
			// Single-row counter serializing event ID assignment across
			// replicas; seeded from MAX(id) so adopting an existing events
			// table keeps IDs monotonic.
			`CREATE TABLE ouvrier_event_counter (
				singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
				last_id BIGINT NOT NULL
			)`,
			`INSERT INTO ouvrier_event_counter (singleton, last_id)
				SELECT TRUE, COALESCE(MAX(id), 0) FROM ouvrier_events`,
			`CREATE TABLE ouvrier_schema_violations (
				id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				at TIMESTAMPTZ NOT NULL,
				exec_id TEXT NOT NULL,
				session_id TEXT NOT NULL,
				schema_name TEXT NOT NULL,
				error TEXT NOT NULL
			)`,
			`CREATE TABLE ouvrier_memory (
				scope TEXT NOT NULL,
				key TEXT NOT NULL,
				value TEXT NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (scope, key)
			)`,
			`CREATE TABLE ouvrier_approvals (
				seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
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
				created_at TIMESTAMPTZ NOT NULL,
				decided_at TIMESTAMPTZ,
				decided_by TEXT NOT NULL
			)`,
			`CREATE INDEX idx_ouvrier_approvals_status ON ouvrier_approvals(status, seq)`,
		},
	},
	{
		// Fenced TTL leases for cron leader election and durable-run recovery
		// claims. Expiry comparisons run against now() so replica clock skew
		// never decides ownership.
		version: 2,
		statements: []string{
			`CREATE TABLE ouvrier_leases (
				name TEXT PRIMARY KEY,
				holder TEXT NOT NULL,
				fence BIGINT NOT NULL,
				acquired_at TIMESTAMPTZ NOT NULL,
				renewed_at TIMESTAMPTZ NOT NULL,
				expires_at TIMESTAMPTZ NOT NULL
			)`,
		},
	},
	{
		// Durable runs (OUVRIER_DURABLE_RUNS=1): step-checkpoint journal and
		// tool intents. Journal input and checkpoint output are redacted
		// before they reach the database; tool intents are metadata only.
		version: 3,
		statements: []string{
			`CREATE TABLE ouvrier_run_journal (
				exec_id TEXT PRIMARY KEY,
				plan_key TEXT NOT NULL,
				plan_hash TEXT NOT NULL,
				trigger_kind TEXT NOT NULL,
				input TEXT NOT NULL,
				created_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX idx_ouvrier_run_journal_created_at ON ouvrier_run_journal(created_at)`,
			`CREATE TABLE ouvrier_run_checkpoints (
				exec_id TEXT NOT NULL,
				step_index BIGINT NOT NULL,
				output TEXT NOT NULL,
				completed_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (exec_id, step_index)
			)`,
			`CREATE TABLE ouvrier_tool_intents (
				exec_id TEXT NOT NULL,
				tool_call_id TEXT NOT NULL,
				step_index BIGINT NOT NULL,
				tool_name TEXT NOT NULL,
				effect TEXT NOT NULL,
				idem_key TEXT NOT NULL,
				started_at TIMESTAMPTZ NOT NULL,
				completed_at TIMESTAMPTZ,
				PRIMARY KEY (exec_id, tool_call_id)
			)`,
		},
	},
	{
		// Durable-run recovery (#40): args_hash on approvals lets a recovered
		// run auto-allow a replayed gated call against its already-approved
		// record. Additive with a default so pre-existing rows stay valid;
		// records written before this migration simply never match.
		version: 4,
		statements: []string{
			`ALTER TABLE ouvrier_approvals ADD COLUMN args_hash TEXT NOT NULL DEFAULT ''`,
		},
	},
	{
		// Outcome-aware idempotency. Existing rows predate outcome tracking,
		// so preserve their at-most-once behavior by treating them as succeeded.
		// ReserveIdempotency explicitly writes pending for every new row.
		version: 5,
		statements: []string{
			`ALTER TABLE ouvrier_idempotency_keys ADD COLUMN outcome TEXT NOT NULL DEFAULT 'succeeded'`,
			`ALTER TABLE ouvrier_idempotency_keys ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now()`,
		},
	},
}

// migrate applies pending migrations inside one transaction, serialized by a
// transaction-scoped advisory lock so concurrent replicas starting against the
// same database (and schema) cannot race the DDL. It returns the versions
// applied by this call (empty when the schema was already current), so
// `ouvrier state migrate` can report what it did.
func (s *PostgresStore) migrate(ctx context.Context) ([]int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres state store: begin migration: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext(current_schema()), hashtext('ouvrier_state_migrations'))`); err != nil {
		return nil, fmt.Errorf("postgres state store: acquire migration lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS ouvrier_schema_migrations (
		version BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("postgres state store: ensure migrations table: %w", err)
	}

	applied := map[int]bool{}
	// rows must be fully consumed and closed before any subsequent
	// ExecContext on the same transaction (single pgx connection); do not
	// switch these explicit Close calls to a defer.
	rows, err := tx.QueryContext(ctx, `SELECT version FROM ouvrier_schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("postgres state store: read applied migrations: %w", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres state store: scan applied migration: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("postgres state store: read applied migrations: %w", err)
	}
	rows.Close()

	var appliedNow []int
	for _, migration := range postgresMigrations {
		if applied[migration.version] {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return nil, fmt.Errorf("postgres state store: apply migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ouvrier_schema_migrations (version) VALUES ($1)`, migration.version); err != nil {
			return nil, fmt.Errorf("postgres state store: record migration %d: %w", migration.version, err)
		}
		appliedNow = append(appliedNow, migration.version)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("postgres state store: commit migrations: %w", err)
	}
	return appliedNow, nil
}

// verifySchema checks that every known migration is recorded as applied,
// without ever creating or mutating schema objects (not even the migrations
// table). It backs OUVRIER_STATE_MIGRATE=off for DML-only roles.
func (s *PostgresStore) verifySchema(ctx context.Context) error {
	pending, err := s.pendingMigrations(ctx)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("postgres state store: %d schema migration(s) pending starting at version %d; run \"ouvrier state migrate\" with a DDL-capable role (%s=%s never runs DDL)",
			len(pending), pending[0], EnvStateMigrate, MigrateOff)
	}
	return nil
}

// pendingMigrations returns the versions in postgresMigrations not yet
// recorded in ouvrier_schema_migrations. A missing migrations table means
// nothing has been applied. to_regclass resolves via search_path, matching
// where the migrations would run.
func (s *PostgresStore) pendingMigrations(ctx context.Context) ([]int, error) {
	var tableExists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT to_regclass('ouvrier_schema_migrations') IS NOT NULL`).Scan(&tableExists); err != nil {
		return nil, fmt.Errorf("postgres state store: check migrations table: %w", err)
	}

	applied := map[int]bool{}
	if tableExists {
		rows, err := s.db.QueryContext(ctx, `SELECT version FROM ouvrier_schema_migrations`)
		if err != nil {
			return nil, fmt.Errorf("postgres state store: read applied migrations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var version int
			if err := rows.Scan(&version); err != nil {
				return nil, fmt.Errorf("postgres state store: scan applied migration: %w", err)
			}
			applied[version] = true
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("postgres state store: read applied migrations: %w", err)
		}
	}

	var pending []int
	for _, migration := range postgresMigrations {
		if !applied[migration.version] {
			pending = append(pending, migration.version)
		}
	}
	return pending, nil
}
