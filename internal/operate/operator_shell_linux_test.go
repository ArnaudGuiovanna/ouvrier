//go:build linux

package operate

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOperatorShellSandboxClearsEnvironmentAndHidesHostRoot(t *testing.T) {
	dir := t.TempDir()
	const secret = "operator-shell-host-secret"
	t.Setenv("OUVRIER_SHELL_SECRET", secret)
	out, truncated, err := runOperatorShellSandbox(context.Background(), dir,
		`test -z "${OUVRIER_SHELL_SECRET:-}" && test ! -e /etc/passwd && printf confined`)
	if err != nil {
		t.Fatalf("runOperatorShellSandbox() error = %v; output=%q", err, out)
	}
	if truncated || out != "confined" || strings.Contains(out, secret) {
		t.Fatalf("sandbox output = %q, truncated=%v", out, truncated)
	}
}

func TestOperatorShellSandboxAllowsOnlyWorkspaceMutation(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runOperatorShellSandbox(context.Background(), dir, `printf governed > generated.txt`)
	if err != nil {
		t.Fatalf("runOperatorShellSandbox() error = %v; output=%q", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "generated.txt"))
	if err != nil || string(data) != "governed" {
		t.Fatalf("workspace mutation = %q, %v", data, err)
	}
}

func TestOperatorShellSandboxDeniesNetwork(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()
	dir := t.TempDir()
	command := fmt.Sprintf(`curl --max-time 1 --fail --silent http://%s/`, listener.Addr())
	if out, _, err := runOperatorShellSandbox(context.Background(), dir, command); err == nil {
		t.Fatalf("network command unexpectedly succeeded: %q", out)
	}
	select {
	case <-accepted:
		t.Fatal("sandbox reached a host-network listener")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestOperatorShellSandboxBoundsOutputAndCancellation(t *testing.T) {
	dir := t.TempDir()
	out, truncated, err := runOperatorShellSandbox(context.Background(), dir, `yes x | head -c 70000`)
	if err != nil {
		t.Fatalf("bounded output command: %v", err)
	}
	if !truncated || len(out) > maxShellOutput+128 || !strings.Contains(out, "truncated") {
		t.Fatalf("bounded output bytes=%d truncated=%v", len(out), truncated)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, _, err := runOperatorShellSandbox(ctx, dir, `sleep 30`); err == nil {
		t.Fatal("cancelled shell command returned nil error")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cancelled shell took %s", elapsed)
	}
}
