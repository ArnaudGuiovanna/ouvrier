//go:build linux

package operate

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultAuditSandboxClearsSecretsProtectsSourceAndDeniesNetwork(t *testing.T) {
	dir := writeWorkerFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module sandbox-worker\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const secretName = "OUVRIER_AUDIT_SANDBOX_SECRET"
	const secretValue = "sandbox-must-not-see-this-value"
	t.Setenv(secretName, secretValue)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("WORKER_SECRET="+secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	testSource := fmt.Sprintf(`package main

import (
	"net"
	"os"
	"testing"
	"time"
)

func TestAuditConfinement(t *testing.T) {
	if value := os.Getenv(%q); value != "" {
		t.Fatalf("inherited process secret: %%q", value)
	}
	if data, err := os.ReadFile(".env"); err == nil {
		t.Fatalf("read worker secret file: %%q", data)
	}
	if err := os.WriteFile("main.go", []byte("mutated"), 0o600); err == nil {
		t.Fatal("worker workspace is writable")
	}
	conn, err := net.DialTimeout("tcp", %q, 300*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("sandbox reached the host network")
	}
}
`, secretName, listener.Addr().String())
	if err := os.WriteFile(filepath.Join(dir, "audit_confinement_test.go"), []byte(testSource), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	report, err := NewAuditRunner().Run(ctx, dir)
	if err != nil {
		t.Fatalf("default sandbox audit: %v", err)
	}
	if !report.Passed {
		t.Fatalf("sandboxed audit failed: %+v", report.Results)
	}
	after, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("sandbox audit changed worker: before=%+v after=%+v", before, after)
	}
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || strings.TrimSpace(string(data)) == "mutated" {
		t.Fatalf("worker main.go was mutated: %q, %v", data, err)
	}
}

func TestDefaultAuditSandboxFailsClosedWhenNamespacesUnavailable(t *testing.T) {
	dir := writeWorkerFixture(t)
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	if err := os.Symlink(goBinary, filepath.Join(pathDir, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	_, err = NewAuditRunner().Run(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "sandbox unavailable") {
		t.Fatalf("audit error = %v, want fail-closed sandbox error", err)
	}
}

func TestDefaultBuildCoordinatorPublishesSanitizedSandboxArtifact(t *testing.T) {
	dir := writeWorkerFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module sandbox-build-worker\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const secret = "final-binary-must-not-contain-this-worker-secret"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("WORKER_SECRET="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainSource := `package main

import (
	"embed"
	"fmt"
)

//go:embed all:*
var source embed.FS

func main() {
	entries, _ := source.ReadDir(".")
	fmt.Print(len(entries))
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainSource), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := stableCandidateSourceSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	artifact, err := (BuildCoordinator{}).Build(ctx, "sandbox-session", dir, "linux/amd64", ProgressWriter{})
	if err != nil {
		t.Fatalf("sandboxed final build: %v", err)
	}
	if artifact.SourceSHA256 != before.SHA256 || artifact.Toolchain != before.Toolchain {
		t.Fatalf("artifact provenance = %+v, want source %+v", artifact, before)
	}
	binary, err := os.ReadFile(artifact.BinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(binary, []byte(secret)) {
		t.Fatal("sandboxed final binary contains the worker .env secret")
	}
	if info, err := os.Stat(artifact.BinaryPath); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("published binary mode = %v, %v", info, err)
	}
	if current, err := stableCandidateSourceSnapshot(dir); err != nil || current != before {
		t.Fatalf("final build changed source: current=%+v err=%v before=%+v", current, err, before)
	}
}
