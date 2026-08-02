package operate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureWorkerStateDirRejectsExternalStateSymlink(t *testing.T) {
	worker := t.TempDir()
	external := t.TempDir()
	makeSymlink(t, external, filepath.Join(worker, ".ouvrier"))

	_, err := ensureWorkerStateDir(worker, "build")
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("ensureWorkerStateDir() error = %v, want symlink rejection", err)
	}
	entries, readErr := os.ReadDir(external)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("external state target received files: %+v", entries)
	}
}

func TestEnsureWorkerStateDirCreatesPrivateContainedDirectory(t *testing.T) {
	worker := t.TempDir()
	dir, err := ensureWorkerStateDir(worker, "audit/builds")
	if err != nil {
		t.Fatalf("ensureWorkerStateDir() error = %v", err)
	}
	if !pathWithinRoot(filepath.Join(worker, ".ouvrier"), dir) {
		t.Fatalf("state dir = %q, want contained path", dir)
	}
	for _, path := range []string{filepath.Join(worker, ".ouvrier"), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s mode = %o, want 700", path, got)
		}
	}
}
