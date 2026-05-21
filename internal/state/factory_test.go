package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStoreFromEnvDefaultsToSQLite(t *testing.T) {
	t.Setenv(EnvStateBackend, "")
	t.Setenv(EnvStatePath, filepath.Join(t.TempDir(), "state.db"))

	store, err := NewStoreFromEnv()
	if err != nil {
		t.Fatalf("NewStoreFromEnv returned error: %v", err)
	}
	defer closeStore(t, store)

	if _, ok := store.(*SQLiteStore); !ok {
		t.Fatalf("store type = %T, want *SQLiteStore", store)
	}
}

func TestNewStoreFromEnvSupportsMemory(t *testing.T) {
	t.Setenv(EnvStateBackend, BackendMemory)

	store, err := NewStoreFromEnv()
	if err != nil {
		t.Fatalf("NewStoreFromEnv returned error: %v", err)
	}
	if _, ok := store.(*MemoryStore); !ok {
		t.Fatalf("store type = %T, want *MemoryStore", store)
	}
}

func TestNewStoreFromEnvRejectsUnknownBackend(t *testing.T) {
	t.Setenv(EnvStateBackend, "redis")

	_, err := NewStoreFromEnv()
	if err == nil || !strings.Contains(err.Error(), `unsupported state backend "redis"`) {
		t.Fatalf("NewStoreFromEnv error = %v", err)
	}
}

func closeStore(t *testing.T, store Store) {
	t.Helper()
	closer, ok := store.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}
