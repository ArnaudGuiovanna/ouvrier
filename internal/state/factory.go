package state

import (
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

	BackendMemory   = "memory"
	BackendSQLite   = "sqlite"
	BackendPostgres = "postgres"
)

func NewStoreFromEnv() (Store, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv(EnvStateBackend)))
	if backend == "" {
		backend = BackendSQLite
	}

	switch backend {
	case BackendSQLite:
		return NewSQLiteStore(os.Getenv(EnvStatePath))
	case BackendMemory:
		return NewMemoryStore(), nil
	case BackendPostgres:
		return NewPostgresStoreFromEnv()
	default:
		return nil, fmt.Errorf("unsupported state backend %q", backend)
	}
}
