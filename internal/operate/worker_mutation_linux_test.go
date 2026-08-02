//go:build linux

package operate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteWorkerFileRejectsParentSymlinkExchange(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	safe := filepath.Join(root, "safe")
	if err := os.Mkdir(safe, 0o755); err != nil {
		t.Fatal(err)
	}

	err := writeWorkerFile(Workspace{Dir: root}, "safe/generated.go", "package generated\n", func() {
		if err := os.Rename(safe, filepath.Join(root, "safe-original")); err != nil {
			t.Fatal(err)
		}
		makeSymlink(t, external, safe)
	})
	if err == nil || !strings.Contains(err.Error(), "anchored worker directory") {
		t.Fatalf("writeWorkerFile() error = %v, want exchanged parent rejection", err)
	}
	assertPathAbsent(t, filepath.Join(external, "generated.go"))
	assertPathAbsent(t, filepath.Join(root, "safe-original", "generated.go"))
}

func TestWriteWorkerFileRejectsProtectedDirectorySymlinkExchange(t *testing.T) {
	root := t.TempDir()
	safe := filepath.Join(root, "safe")
	state := filepath.Join(root, ".ouvrier")
	if err := os.Mkdir(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(state, "session.json")
	if err := os.WriteFile(stateFile, []byte("protected\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := writeWorkerFile(Workspace{Dir: root}, "safe/session.json", "attacker\n", func() {
		if err := os.Rename(safe, filepath.Join(root, "safe-original")); err != nil {
			t.Fatal(err)
		}
		makeSymlink(t, ".ouvrier", safe)
	})
	if err == nil {
		t.Fatal("writeWorkerFile accepted a post-validation pivot into .ouvrier")
	}
	assertFileContent(t, stateFile, "protected\n")
}

func TestWriteWorkerFileRejectsFinalSymlinkExchangeWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(external, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "worker.go")
	if err := os.WriteFile(target, []byte("package original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := writeWorkerFile(Workspace{Dir: root}, "worker.go", "package changed\n", func() {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		makeSymlink(t, external, target)
	})
	if err == nil || !strings.Contains(err.Error(), "changed after path validation") {
		t.Fatalf("writeWorkerFile() error = %v, want final symlink rejection", err)
	}
	assertFileContent(t, external, "outside\n")
}

func TestRemoveWorkerFileRejectsParentSymlinkExchange(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	safe := filepath.Join(root, "safe")
	if err := os.Mkdir(safe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(safe, "victim.go"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	externalFile := filepath.Join(external, "victim.go")
	if err := os.WriteFile(externalFile, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareWorkerRemoval(Workspace{Dir: root}, "safe/victim.go")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := commitWorkerRemoval(prepared, func() {
		if err := os.Rename(safe, filepath.Join(root, "safe-original")); err != nil {
			t.Fatal(err)
		}
		makeSymlink(t, external, safe)
	})
	if err == nil || removed {
		t.Fatalf("commitWorkerRemoval() = %t, %v, want exchanged parent rejection", removed, err)
	}
	assertFileContent(t, externalFile, "outside\n")
	assertFileContent(t, filepath.Join(root, "safe-original", "victim.go"), "inside\n")
}

func TestRemoveWorkerFileRejectsFinalSymlinkExchangeWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "victim.go")
	if err := os.WriteFile(target, []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(external, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareWorkerRemoval(Workspace{Dir: root}, "victim.go")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := commitWorkerRemoval(prepared, func() {
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		makeSymlink(t, external, target)
	})
	if err == nil || removed {
		t.Fatalf("commitWorkerRemoval() = %t, %v, want exchanged final rejection", removed, err)
	}
	assertFileContent(t, external, "outside\n")
	if info, statErr := os.Lstat(target); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("exchanged symlink was unexpectedly removed: info=%v err=%v", info, statErr)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or returned unexpected error: %v", path, err)
	}
}
