package state

import (
	"fmt"
	"os"
	"strings"
)

const (
	EnvStateBackend = "OUVRIER_STATE_BACKEND"
	EnvStatePath    = "OUVRIER_STATE_PATH"

	BackendMemory = "memory"
	BackendSQLite = "sqlite"
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
	default:
		return nil, fmt.Errorf("unsupported state backend %q", backend)
	}
}
