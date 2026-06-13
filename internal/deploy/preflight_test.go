package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightEnvFilePassesWhenComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	writeFile(t, path, "OUVRIER_ADMIN_TOKEN=tok\nANTHROPIC_API_KEY=key\n")

	if err := PreflightEnvFile(context.Background(), dir, path, []string{"ANTHROPIC_API_KEY"}); err != nil {
		t.Fatalf("PreflightEnvFile() error = %v", err)
	}
}

func TestPreflightEnvFileReportsMissingNamesNeverValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	writeFile(t, path, "PRESENT=super-secret-value\nEMPTY=\n")

	err := PreflightEnvFile(context.Background(), dir, path, []string{"ANTHROPIC_API_KEY", "EMPTY", "PRESENT"})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("PreflightEnvFile() error = %v, want ErrDeploy", err)
	}
	msg := err.Error()
	// Sorted missing names: the absent key, the empty key, and the
	// always-required admin token. The present key must not be listed.
	if !strings.Contains(msg, "ANTHROPIC_API_KEY, EMPTY, OUVRIER_ADMIN_TOKEN") {
		t.Fatalf("missing names not reported (sorted): %v", err)
	}
	if strings.Contains(msg, "super-secret-value") {
		t.Fatalf("env value leaked into error: %v", err)
	}
	if strings.Contains(msg, "PRESENT,") || strings.HasSuffix(msg, "PRESENT") {
		t.Fatalf("present key reported missing: %v", err)
	}
}

func TestPreflightEnvFileAlwaysRequiresAdminToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	writeFile(t, path, "ANTHROPIC_API_KEY=key\n")

	err := PreflightEnvFile(context.Background(), dir, path, nil)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("PreflightEnvFile() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "OUVRIER_ADMIN_TOKEN") {
		t.Fatalf("admin token requirement not reported: %v", err)
	}
}

func TestPreflightEnvFileMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	err := PreflightEnvFile(context.Background(), dir, filepath.Join(dir, ".env"), nil)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("PreflightEnvFile() error = %v, want ErrDeploy", err)
	}
}

func TestPreflightEnvFileRefusesGitTrackedFile(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	writeFile(t, path, "OUVRIER_ADMIN_TOKEN=tok\n")
	gitInit(t, dir)
	gitRun(t, dir, "add", "-f", ".env")

	err := PreflightEnvFile(context.Background(), dir, path, nil)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("PreflightEnvFile() error = %v, want ErrDeploy", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "tracked by git") {
		t.Fatalf("git-tracked refusal expected, got: %v", err)
	}
	if strings.Contains(msg, "tok") && !strings.Contains(msg, "token") {
		t.Fatalf("value leaked into error: %v", err)
	}
}

func TestPreflightEnvFileUntrackedInGitRepoPasses(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	writeFile(t, path, "OUVRIER_ADMIN_TOKEN=tok\n")
	gitInit(t, dir)
	// .env exists but is never added.

	if err := PreflightEnvFile(context.Background(), dir, path, nil); err != nil {
		t.Fatalf("PreflightEnvFile() error = %v, want nil for untracked file", err)
	}
}

func TestPreflightEnvFileNonGitDirectoryTreatedAsUntracked(t *testing.T) {
	dir := t.TempDir() // no git repo here
	path := filepath.Join(dir, ".env")
	writeFile(t, path, "OUVRIER_ADMIN_TOKEN=tok\n")

	if err := PreflightEnvFile(context.Background(), dir, path, nil); err != nil {
		t.Fatalf("PreflightEnvFile() error = %v, want nil outside a git repo", err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
