// Package envnames is the single source of truth for the runtime environment
// variable names read by Ouvrier. Legacy PIP_* names are listed only so the
// startup guard can fail loudly when one is still set; they are never read.
package envnames

const (
	Env        = "OUVRIER_ENV"
	AdminToken = "OUVRIER_ADMIN_TOKEN"
	Addr       = "OUVRIER_ADDR"
	LogLevel   = "OUVRIER_LOG_LEVEL"

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
