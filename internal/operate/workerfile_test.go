package operate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWriteWorkerFileSandbox(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := Workspace{Dir: dir, Name: "demo"}
	got, err := ReadWorkerFile(ws, "main.go")
	if err != nil || got != "package main\n" {
		t.Fatalf("read: %q err=%v", got, err)
	}
	if err := WriteWorkerFile(ws, "main.go", "package main // edited\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(data) != "package main // edited\n" {
		t.Fatalf("not written: %s", data)
	}
	if _, err := ReadWorkerFile(ws, "../secret"); err == nil {
		t.Fatal("expected rejection of ../ path")
	}
	if err := WriteWorkerFile(ws, "/etc/passwd", "x"); err == nil {
		t.Fatal("expected rejection of absolute path")
	}
}

func TestWorkerFileRejectsSymlinkOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	secret := filepath.Join(external, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "secret.txt")
	makeSymlink(t, secret, link)
	ws := Workspace{Dir: root, Name: "demo"}

	if _, err := ReadWorkerFile(ws, "secret.txt"); err == nil || !strings.Contains(err.Error(), "unsafe worker file path") {
		t.Fatalf("ReadWorkerFile() error = %v, want unsafe symlink rejection", err)
	}
	if err := WriteWorkerFile(ws, "secret.txt", "changed"); err == nil || !strings.Contains(err.Error(), "unsafe worker file path") {
		t.Fatalf("WriteWorkerFile() error = %v, want unsafe symlink rejection", err)
	}
	data, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "outside" {
		t.Fatalf("external file = %q, want unchanged", data)
	}
}

func TestWriteWorkerFileRejectsExternalSymlinkParentForNewFile(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	makeSymlink(t, external, filepath.Join(root, "escape"))

	err := WriteWorkerFile(Workspace{Dir: root}, "escape/new.go", "package escaped\n")
	if err == nil || !strings.Contains(err.Error(), "unsafe worker file path") {
		t.Fatalf("WriteWorkerFile() error = %v, want external parent rejection", err)
	}
	if _, err := os.Stat(filepath.Join(external, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("external file stat error = %v, want no file", err)
	}
}

func TestWorkerFileResolvesInternalSymlinkBeforeWriting(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(realDir, "worker.go")
	if err := os.WriteFile(realFile, []byte("package original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "worker.go")
	makeSymlink(t, realFile, link)
	ws := Workspace{Dir: root}

	got, err := ReadWorkerFile(ws, "worker.go")
	if err != nil || got != "package original\n" {
		t.Fatalf("ReadWorkerFile() = %q, %v", got, err)
	}
	if err := WriteWorkerFile(ws, "worker.go", "package updated\n"); err != nil {
		t.Fatalf("WriteWorkerFile() error = %v", err)
	}
	data, err := os.ReadFile(realFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package updated\n" {
		t.Fatalf("real file = %q, want updated content", data)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("WriteWorkerFile replaced the in-workspace symlink instead of resolving it")
	}
}

func TestWriteWorkerFileAllowsNewNestedPathUnderRealWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := WriteWorkerFile(Workspace{Dir: root}, "tools/generated/helper.go", "package generated\n"); err != nil {
		t.Fatalf("WriteWorkerFile() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "tools", "generated", "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package generated\n" {
		t.Fatalf("generated file = %q", data)
	}
}

func TestWorkerFileRejectsCockpitStateDirectory(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".ouvrier")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, "session.json")
	if err := os.WriteFile(stateFile, []byte("secret state"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := Workspace{Dir: root}

	if _, err := ReadWorkerFile(ws, ".ouvrier/session.json"); err == nil {
		t.Fatal("ReadWorkerFile exposed cockpit state")
	}
	if err := WriteWorkerFile(ws, ".ouvrier/session.json", "overwritten"); err == nil {
		t.Fatal("WriteWorkerFile modified cockpit state")
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "secret state" {
		t.Fatalf("cockpit state = %q, want unchanged", data)
	}

	makeSymlink(t, stateDir, filepath.Join(root, "state-link"))
	if _, err := ReadWorkerFile(ws, "state-link/session.json"); err == nil {
		t.Fatal("ReadWorkerFile followed a symlink into cockpit state")
	}
}

func TestWriteWorkerFileBoundsTextContent(t *testing.T) {
	ws := Workspace{Dir: t.TempDir()}
	if err := WriteWorkerFile(ws, "too-large.txt", strings.Repeat("x", maxWorkerFileBytes+1)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized WriteWorkerFile() error = %v, want size rejection", err)
	}
	if err := WriteWorkerFile(ws, "invalid.txt", string([]byte{0xff})); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 WriteWorkerFile() error = %v, want UTF-8 rejection", err)
	}
}

func makeSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
}
