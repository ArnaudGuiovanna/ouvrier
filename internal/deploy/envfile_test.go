package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestResolveEnvFilePrefersPerEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "A=1\n")
	writeFile(t, filepath.Join(dir, ".env.staging"), "A=2\n")

	got, err := ResolveEnvFile(dir, "staging", "")
	if err != nil {
		t.Fatalf("ResolveEnvFile() error = %v", err)
	}
	if got != filepath.Join(dir, ".env.staging") {
		t.Fatalf("ResolveEnvFile() = %q, want .env.staging", got)
	}
}

func TestResolveEnvFileFallsBackToDotEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "A=1\n")

	got, err := ResolveEnvFile(dir, "prod", "")
	if err != nil {
		t.Fatalf("ResolveEnvFile() error = %v", err)
	}
	if got != filepath.Join(dir, ".env") {
		t.Fatalf("ResolveEnvFile() = %q, want .env", got)
	}
}

func TestResolveEnvFileOverrideWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "A=1\n")
	writeFile(t, filepath.Join(dir, ".env.prod"), "A=2\n")
	writeFile(t, filepath.Join(dir, "ci.env"), "A=3\n")

	got, err := ResolveEnvFile(dir, "prod", "ci.env")
	if err != nil {
		t.Fatalf("ResolveEnvFile() error = %v", err)
	}
	if got != filepath.Join(dir, "ci.env") {
		t.Fatalf("ResolveEnvFile() = %q, want ci.env", got)
	}
}

func TestResolveEnvFileOverrideAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "secrets.env")
	writeFile(t, other, "A=1\n")

	got, err := ResolveEnvFile(dir, "prod", other)
	if err != nil {
		t.Fatalf("ResolveEnvFile() error = %v", err)
	}
	if got != other {
		t.Fatalf("ResolveEnvFile() = %q, want %q", got, other)
	}
}

func TestResolveEnvFileOverrideMustExist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "A=1\n")

	_, err := ResolveEnvFile(dir, "prod", "missing.env")
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("ResolveEnvFile() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "missing.env") {
		t.Fatalf("error should name the override file, got: %v", err)
	}
}

func TestResolveEnvFileMissingNamesCandidates(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveEnvFile(dir, "staging", "")
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("ResolveEnvFile() error = %v, want ErrDeploy", err)
	}
	msg := err.Error()
	for _, want := range []string{".env.staging", ".env", "--env-file", "OUVRIER_DEPLOY_ENV_FILE"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q in: %v", want, err)
		}
	}
}

func TestResolveEnvFileNoEnvNameSkipsPerEnvCandidate(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveEnvFile(dir, "", "")
	if err == nil {
		t.Fatal("ResolveEnvFile() error = nil, want missing-file error")
	}
	if strings.Contains(err.Error(), ".env.") {
		t.Fatalf("no per-env candidate expected without env name, got: %v", err)
	}
}
