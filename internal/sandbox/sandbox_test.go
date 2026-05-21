package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRequiresExistingWorkspaceDirectory(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("New returned nil error for missing workspace")
	}

	file := filepath.Join(t.TempDir(), "workspace")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, err = New(file)
	if err == nil {
		t.Fatal("New returned nil error for file workspace")
	}
}

func TestResolveAllowsPathsInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	sandbox, err := New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	got, err := sandbox.Resolve("logs/app.log")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	want := filepath.Join(sandbox.Root(), "logs", "app.log")
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
}

func TestResolveRejectsTraversalOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	sandbox, err := New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = sandbox.Resolve("../outside.txt")
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Resolve error = %v, want ErrPathEscape", err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sandbox, err := New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	_, err = sandbox.Resolve("outside/secret.txt")
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Resolve error = %v, want ErrPathEscape", err)
	}
}

func TestEnvironmentUsesExplicitAllowlist(t *testing.T) {
	root := t.TempDir()
	sandbox, err := New(root,
		WithEnvironment(map[string]string{
			"PATH":   "/usr/bin",
			"SECRET": "hidden",
			"EMPTY":  "",
		}),
		WithAllowedEnv("PATH", "EMPTY"),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	env := sandbox.Environment()
	if env["PWD"] != sandbox.Root() {
		t.Fatalf("PWD = %q, want sandbox root", env["PWD"])
	}
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("PATH = %q, want /usr/bin", env["PATH"])
	}
	if _, ok := env["EMPTY"]; !ok {
		t.Fatal("EMPTY was not preserved by allowlist")
	}
	if _, ok := env["SECRET"]; ok {
		t.Fatal("SECRET leaked into sandbox environment")
	}
}
