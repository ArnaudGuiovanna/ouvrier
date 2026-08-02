//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicRejectsDeterministicParentSymlinkPivot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	originalParent := filepath.Join(root, "out")
	if err := os.Mkdir(originalParent, 0o755); err != nil {
		t.Fatalf("Mkdir(out): %v", err)
	}
	externalTarget := filepath.Join(outside, "result.json")
	if err := os.WriteFile(externalTarget, []byte("outside-safe"), 0o600); err != nil {
		t.Fatalf("seed outside target: %v", err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = s.writeFileAtomic("out/result.json", []byte("attacker-controlled"), 0o644, func() {
		if renameErr := os.Rename(originalParent, filepath.Join(root, "out-original")); renameErr != nil {
			t.Fatalf("pivot rename: %v", renameErr)
		}
		if symlinkErr := os.Symlink(outside, originalParent); symlinkErr != nil {
			t.Fatalf("pivot symlink: %v", symlinkErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("writeFileAtomic error = %v, want fail-closed sandbox pivot error", err)
	}
	data, readErr := os.ReadFile(externalTarget)
	if readErr != nil || string(data) != "outside-safe" {
		t.Fatalf("outside target = %q err=%v, want unchanged", data, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "out-original", "result.json")); !os.IsNotExist(statErr) {
		t.Fatalf("anchored old parent unexpectedly received output: %v", statErr)
	}
}

func TestWriteFileAtomicRejectsFinalSymlinkAndReplacesRegularFile(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("outside-safe"), 0o600); err != nil {
		t.Fatalf("seed outside: %v", err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.json")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := s.WriteFileAtomic("linked.json", []byte("escape"), 0o644); err == nil {
		t.Fatal("WriteFileAtomic accepted final symlink")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside-safe" {
		t.Fatalf("outside after final symlink = %q err=%v", data, err)
	}

	target := filepath.Join(root, "result.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed regular target: %v", err)
	}
	if err := s.WriteFileAtomic(target, []byte("new-complete"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic regular file: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "new-complete" {
		t.Fatalf("regular target = %q err=%v", data, err)
	}
	matches, err := filepath.Glob(filepath.Join(root, ".ouvrier-sink-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files = %v err=%v, want none", matches, err)
	}
}
