package state

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

const (
	EnvStateBackend  = envnames.StateBackend
	EnvStatePath     = envnames.StatePath
	EnvStateDSN      = envnames.StateDSN
	EnvStateMaxConns = envnames.StateMaxConns
	EnvStateMigrate  = envnames.StateMigrate

	BackendMemory   = "memory"
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"

	// MigrateAuto (the default) applies pending schema migrations at
	// startup. MigrateOff only verifies the schema version and refuses to
	// start while migrations are pending — it never runs DDL, so the worker
	// can connect with a DML-only role.
	MigrateAuto = "auto"
	MigrateOff  = "off"
)

func NewStoreFromEnv() (Store, error) {
	migrateMode, err := migrateModeFromEnv()
	if err != nil {
		return nil, err
	}

	switch backend := backendFromEnv(); backend {
	case BackendSQLite:
		return newSQLiteStore(context.Background(), os.Getenv(EnvStatePath), migrateMode)
	case BackendMemory:
		return NewMemoryStore(), nil
	case BackendPostgres:
		return newPostgresStoreFromEnv(migrateMode)
	default:
		return nil, fmt.Errorf("unsupported state backend %q", backend)
	}
}

func backendFromEnv() string {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv(EnvStateBackend)))
	if backend == "" {
		return BackendSQLite
	}
	return backend
}

func migrateModeFromEnv() (string, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvStateMigrate)))
	switch mode {
	case "":
		return MigrateAuto, nil
	case MigrateAuto, MigrateOff:
		return mode, nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", EnvStateMigrate, MigrateAuto, MigrateOff, mode)
	}
}
