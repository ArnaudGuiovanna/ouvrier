// Package envnames is the single source of truth for the runtime environment
// variable names read by Ouvrier. Legacy PIP_* names are listed only so the
// startup guard can fail loudly when one is still set; they are never read.
package envnames

const (
	Env        = "OUVRIER_ENV"
	AdminToken = "OUVRIER_ADMIN_TOKEN"
	Addr       = "OUVRIER_ADDR"
	LogLevel   = "OUVRIER_LOG_LEVEL"

	// AdminAddr moves the admin surface (/admin/*, /metrics, and the dev-mode
	// /dev viewer) off the public port onto a dedicated listener bound at this
	// address (e.g. 127.0.0.1:9090); the public port then answers 404 for
	// those routes. Unset preserves the v0.2 shared-port layout. MetricsPublic
	// ("1") additionally keeps /metrics registered on the public port when the
	// surface is split, for Prometheus scrapers that cannot reach the loopback
	// admin listener; it changes nothing while AdminAddr is unset.
	AdminAddr     = "OUVRIER_ADMIN_ADDR"
	MetricsPublic = "OUVRIER_METRICS_PUBLIC"

	StateBackend  = "OUVRIER_STATE_BACKEND"
	StatePath     = "OUVRIER_STATE_PATH"
	StateDSN      = "OUVRIER_STATE_DSN"
	StateMaxConns = "OUVRIER_STATE_MAX_CONNS"
	StateMigrate  = "OUVRIER_STATE_MIGRATE"

	// ReplicaID overrides the generated <hostname>-<rand8> cron lease holder
	// identity. CronLease set to "off" disables cron leader-leases even when
	// the state backend supports them.
	ReplicaID = "OUVRIER_REPLICA_ID"
	CronLease = "OUVRIER_CRON_LEASE"

	// DurableRuns ("1") opts in to the crash-safe step-checkpoint run journal;
	// default off. DurableRetention bounds how long failed/suspended run
	// journals are kept before pruning (Go duration, default 72h).
	DurableRuns      = "OUVRIER_DURABLE_RUNS"
	DurableRetention = "OUVRIER_DURABLE_RETENTION"

	// DeployEnvFile overrides which dotenv file `ouvrier deploy` ships,
	// taking precedence over .env.<env> and .env (same as --env-file).
	DeployEnvFile = "OUVRIER_DEPLOY_ENV_FILE"
	// ConfigDir overrides the user-level config directory (default
	// ~/.config/ouvrier); FleetPath overrides the full path of the
	// deployments inventory (default <config dir>/deployments.json).
	ConfigDir = "OUVRIER_CONFIG_DIR"
	FleetPath = "OUVRIER_FLEET_PATH"

	LegacyEnv        = "PIP_ENV"
	LegacyAdminToken = "PIP_ADMIN_TOKEN"
	LegacyAddr       = "PIP_ADDR"
	LegacyLogLevel   = "PIP_LOG_LEVEL"
)

// Legacy maps each retired name to its replacement, for the startup guard
// and its error messages.
var Legacy = map[string]string{
	LegacyEnv:        Env,
	LegacyAdminToken: AdminToken,
	LegacyAddr:       Addr,
	LegacyLogLevel:   LogLevel,
}
