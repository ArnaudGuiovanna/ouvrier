package operate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditRunUsesProductionRedactorForKnownDotenvSecret(t *testing.T) {
	dir := writeWorkerFixture(t)
	const secret = "dotenv-secret-without-token-shape"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OUVRIER_TEST_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSecretScanFile(t, dir, "nested/leak.go", secret)
	runner := AuditRunner{
		RunCommand: func(context.Context, string, string, []string) (string, string, error) { return "", "", nil },
		Build:      func(context.Context, string, io.Writer, io.Writer) error { return nil },
	}
	report, err := runner.Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("AuditRunner.Run() error = %v", err)
	}
	if report.Passed {
		t.Fatalf("audit unexpectedly passed: %+v", report.Results)
	}
	for _, gate := range report.Results {
		if strings.Contains(gate.Output+gate.Error, secret) {
			t.Fatalf("audit gate leaked production secret: %+v", gate)
		}
	}
}

func TestAuditSecretScanCoversTrackedStagedUnstagedAndUntrackedSource(t *testing.T) {
	const secret = "audit-known-secret-material"
	cases := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "tracked",
			prepare: func(t *testing.T, dir string) {
				writeSecretScanFile(t, dir, "nested/tracked.go", secret)
				gitInitAndCommit(t, dir)
			},
		},
		{
			name: "staged",
			prepare: func(t *testing.T, dir string) {
				gitInitAndCommit(t, dir)
				writeSecretScanFile(t, dir, "nested/staged.go", secret)
				runGitTestCommand(t, dir, "add", "nested/staged.go")
			},
		},
		{
			name: "unstaged",
			prepare: func(t *testing.T, dir string) {
				writeSecretScanFile(t, dir, "nested/unstaged.go", "placeholder-before-commit")
				gitInitAndCommit(t, dir)
				writeSecretScanFile(t, dir, "nested/unstaged.go", secret)
			},
		},
		{
			name: "untracked",
			prepare: func(t *testing.T, dir string) {
				gitInitAndCommit(t, dir)
				writeSecretScanFile(t, dir, "nested/untracked.go", secret)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeWorkerFixture(t)
			tc.prepare(t, dir)
			runner := AuditRunner{Redactor: NewRedactor(secret)}
			result := runner.gateSecretScan(context.Background(), dir)
			if result.Status != GateFail {
				t.Fatalf("secret scan = %+v, want fail", result)
			}
			if strings.Contains(result.Error+result.Output, secret) {
				t.Fatalf("secret scan diagnostic leaked known secret: %+v", result)
			}
		})
	}
}

func TestAuditSecretScanDetectsUnknownCredentialShapeInNestedSource(t *testing.T) {
	dir := writeWorkerFixture(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz012345"
	writeSecretScanFile(t, dir, "internal/deep/config.go", token)

	result := (AuditRunner{}).gateSecretScan(context.Background(), dir)
	if result.Status != GateFail {
		t.Fatalf("secret scan = %+v, want fail", result)
	}
	if strings.Contains(result.Error+result.Output, token) {
		t.Fatalf("secret scan diagnostic leaked token: %+v", result)
	}
}

func TestAuditSecretScanDoesNotRejectSchemaOrExplicitPlaceholder(t *testing.T) {
	dir := writeWorkerFixture(t)
	content := "package main\n\nconst schema = `{" +
		`\"type\":\"object\",\"properties\":{\"secret\":{\"type\":\"string\"},\"password\":\"placeholder\"}}` + "`\n"
	if err := os.WriteFile(filepath.Join(dir, "schema.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	result := (AuditRunner{}).gateSecretScan(context.Background(), dir)
	if result.Status != GatePass {
		t.Fatalf("secret scan = %+v, harmless schema must pass", result)
	}
}

func TestAuditSecretScanFailsClosedAtTreeBoundary(t *testing.T) {
	t.Run("oversized file", func(t *testing.T) {
		dir := writeWorkerFixture(t)
		path := filepath.Join(dir, "oversized.asset")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate((4 << 20) + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}

		result := (AuditRunner{}).gateSecretScan(context.Background(), dir)
		if result.Status != GateFail || !strings.Contains(result.Error, "bounded") {
			t.Fatalf("secret scan = %+v, want explicit bounded failure", result)
		}
	})

	t.Run("escaping symlink", func(t *testing.T) {
		dir := writeWorkerFixture(t)
		external := filepath.Join(t.TempDir(), "outside.go")
		if err := os.WriteFile(external, []byte("package outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(dir, "outside.go")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		result := (AuditRunner{}).gateSecretScan(context.Background(), dir)
		if result.Status != GateFail || !strings.Contains(result.Error, "safely inspect") {
			t.Fatalf("secret scan = %+v, want escaping symlink failure", result)
		}
	})
}

func writeSecretScanFile(t *testing.T, dir, rel, secret string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "package nested\n\nconst embeddedCredential = \"" + secret + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
