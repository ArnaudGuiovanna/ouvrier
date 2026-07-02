package operate

import (
	"os"
	"path/filepath"
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
