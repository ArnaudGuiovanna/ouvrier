package operate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRejectsUntrustedSessionIDs(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	for _, id := range []string{"", "../outside", "0123456789ABCDEf", "0123456789abcde", "0123456789abcdef0"} {
		if _, err := store.Load(id); err == nil {
			t.Fatalf("Load(%q) succeeded", id)
		}
		if got := store.SessionDir(id); got != "" {
			t.Fatalf("SessionDir(%q) = %q, want empty", id, got)
		}
	}
}

func TestStoreRebasesPersistedArtifactPaths(t *testing.T) {
	root := t.TempDir()
	workspace := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	session, err := store.Create(workspace, "manual", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	statePath := filepath.Join(store.SessionDir(session.ID), "session.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	persisted["audit_path"] = filepath.Join(t.TempDir(), "stolen.json")
	persisted["transcript_path"] = filepath.Join(t.TempDir(), "stolen.jsonl")
	persisted["checkpoints_dir"] = t.TempDir()
	data, err = json.Marshal(persisted)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, path := range []string{loaded.AuditPath, loaded.TranscriptPath, loaded.CheckpointsDir} {
		if !strings.HasPrefix(path, store.SessionDir(session.ID)+string(filepath.Separator)) {
			t.Fatalf("rebased path %q escaped session directory", path)
		}
	}
}

func TestStoreRejectsPersistedSessionIDMismatch(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	session, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	path := filepath.Join(store.SessionDir(session.ID), "session.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	persisted["id"] = "aaaaaaaaaaaaaaaa"
	data, _ = json.Marshal(persisted)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := store.Load(session.ID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Load() error = %v, want id mismatch", err)
	}
}

func TestNewStoreRejectsSymlinkedStateRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".ouvrier")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	if _, err := NewStore(root); err == nil {
		t.Fatal("NewStore() accepted a symlinked .ouvrier state root")
	}
}
