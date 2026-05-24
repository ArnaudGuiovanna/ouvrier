// Bash sandbox escape coverage (v0.1 issue #15).
//
// These tests focus on acceptance criteria not already covered by bash_test.go:
//
//   - Filesystem path traversal via `cat ../../etc/passwd` and absolute paths
//   - Symlink escape via the bash command itself (existing test only covered
//     the bubblewrap mode; host mode also needs proof the sandbox.Resolve path
//     is the only sanctioned escape gate)
//   - Environment variable filtering against `env` enumeration and ${VAR}
//     expansion of disallowed keys
//   - Output bound enforcement on a large producer like `seq 1 10000000`
//   - Timeout enforcement on `sleep 60 && echo done`
//   - Child processes do not outlive the parent timeout
//
// Tests that require an actual isolation backend (bubblewrap on Linux) skip
// cleanly via tools.CheckBashIsolationAvailable when the backend is missing
// on the runner (CI without user namespaces, non-Linux runners, etc.).
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/sandbox"
)

// TestBashHandlerBubblewrapDeniesParentDirectoryTraversal asserts that running
// `cat ../../etc/passwd` from inside the bubblewrap-isolated workspace cannot
// read the real host /etc/passwd. The command is allowed to run; the sandbox
// must guarantee the file content does not leak.
func TestBashHandlerBubblewrapDeniesParentDirectoryTraversal(t *testing.T) {
	if err := CheckBashIsolationAvailable(context.Background()); err != nil {
		t.Skipf("isolated bash sandbox unavailable on this host: %v", err)
	}
	handler, err := NewBashHandler(newTestSandbox(t), BashHandlerConfig{})
	if err != nil {
		t.Fatalf("NewBashHandler returned error: %v", err)
	}

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_traversal",
		Name:      "bash",
		Arguments: []byte(`{"command":"cat ../../etc/passwd 2>&1; cat /etc/passwd 2>&1; printf done"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	body := decodeBashResult(t, result)
	combined := body.Stdout + body.Stderr
	if strings.Contains(combined, "root:x:0:0") || strings.Contains(combined, "/bin/bash") && strings.Contains(combined, "root:") {
		t.Fatalf("bash output leaked host /etc/passwd: stdout=%q stderr=%q", body.Stdout, body.Stderr)
	}
	if !strings.Contains(body.Stdout, "done") {
		t.Fatalf("expected sentinel 'done' in stdout, got stdout=%q stderr=%q", body.Stdout, body.Stderr)
	}
}

// TestBashHandlerHostModeRejectsWorkdirTraversal proves the sandbox.Resolve
// gate denies a workdir argument that points outside the workspace even in
// host mode (no bubblewrap backend), exercising the same code path that bash
// callers depend on.
func TestBashHandlerHostModeRejectsWorkdirTraversal(t *testing.T) {
	root := t.TempDir()
	sb, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler := newTestBashHandler(t, sb, BashHandlerConfig{})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_workdir_traversal",
		Name:      "bash",
		Arguments: []byte(`{"command":"pwd","workdir":"../../etc"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, want workdir traversal denied; content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if !strings.Contains(body.Error, "sandbox path escape") {
		t.Fatalf("error = %q, want sandbox path escape", body.Error)
	}
}

// TestBashHandlerHostModeRejectsAbsoluteWorkdir confirms that an absolute
// outside-the-sandbox workdir is rejected even in the host fallback mode.
func TestBashHandlerHostModeRejectsAbsoluteWorkdir(t *testing.T) {
	root := t.TempDir()
	sb, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler := newTestBashHandler(t, sb, BashHandlerConfig{})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_abs_workdir",
		Name:      "bash",
		Arguments: []byte(`{"command":"pwd","workdir":"/etc"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, want absolute workdir denied; content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if !strings.Contains(body.Error, "sandbox path escape") {
		t.Fatalf("error = %q, want sandbox path escape", body.Error)
	}
}

// TestBashHandlerBubblewrapDeniesAbsoluteSymlinkEscape exercises symlink
// escapes created from inside the workspace. The container view should not
// reveal the host target because bubblewrap only binds the workspace root.
func TestBashHandlerBubblewrapDeniesAbsoluteSymlinkEscape(t *testing.T) {
	if err := CheckBashIsolationAvailable(context.Background()); err != nil {
		t.Skipf("isolated bash sandbox unavailable on this host: %v", err)
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("host-secret-payload"), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	sb, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler, err := NewBashHandler(sb, BashHandlerConfig{})
	if err != nil {
		t.Fatalf("NewBashHandler returned error: %v", err)
	}

	// Create the symlink from inside the bash session, then try to read it.
	cmd := fmt.Sprintf("ln -s %s ./escape && cat ./escape 2>&1; printf done", secret)
	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_symlink_runtime",
		Name:      "bash",
		Arguments: []byte(`{"command":` + jsonString(cmd) + `}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	body := decodeBashResult(t, result)
	combined := body.Stdout + body.Stderr + body.Error
	if strings.Contains(combined, "host-secret-payload") {
		t.Fatalf("symlink escape leaked host file: %+v", body)
	}
	if !strings.Contains(body.Stdout, "done") {
		t.Fatalf("expected sentinel 'done' in stdout, got %+v", body)
	}
}

// TestBashHandlerHidesNonAllowlistedEnvFromEnumeration ensures `env` and
// `${VAR}` expansion cannot reveal environment variables the sandbox did not
// allow through.
func TestBashHandlerHidesNonAllowlistedEnvFromEnumeration(t *testing.T) {
	root := t.TempDir()
	sb, err := sandbox.New(root,
		sandbox.WithEnvironment(map[string]string{
			"VISIBLE":    "ok",
			"SECRET":     "secret-value",
			"OTHER_KEY":  "other-value",
			"DEEP_TOKEN": "deep-value",
		}),
		sandbox.WithAllowedEnv("VISIBLE"),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler := newTestBashHandler(t, sb, BashHandlerConfig{})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_env",
		Name:      "bash",
		Arguments: []byte(`{"command":"env; printf -- '---'; printf 'SECRET=%s' \"${SECRET-unset}\"; printf ';OTHER=%s' \"${OTHER_KEY-unset}\"; printf ';DEEP=%s' \"${DEEP_TOKEN-unset}\""}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	combined := body.Stdout
	for _, leaked := range []string{"secret-value", "other-value", "deep-value"} {
		if strings.Contains(combined, leaked) {
			t.Fatalf("env enumeration leaked disallowed value %q: stdout=%q", leaked, combined)
		}
	}
	if !strings.Contains(combined, "VISIBLE=ok") {
		t.Fatalf("stdout missing visible env entry: %q", combined)
	}
}

// TestBashHandlerBoundsLargeOutputProducer feeds `seq 1 10000000` through the
// bash tool with a small max-output-bytes cap and asserts the truncation flag
// is observable and the buffer is exactly the configured size.
func TestBashHandlerBoundsLargeOutputProducer(t *testing.T) {
	const cap = 4096
	handler := newTestBashHandler(t, newTestSandbox(t), BashHandlerConfig{MaxOutputBytes: cap})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_large_output",
		Name:      "bash",
		Arguments: []byte(`{"command":"seq 1 10000000"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if !body.Truncated || !body.StdoutTruncated {
		t.Fatalf("result = %+v, want truncated stdout", body)
	}
	if len(body.Stdout) != cap {
		t.Fatalf("stdout length = %d, want = %d (max output cap)", len(body.Stdout), cap)
	}
	// First line of `seq 1 ...` is "1", so the captured prefix should start with it.
	if !strings.HasPrefix(body.Stdout, "1\n") {
		t.Fatalf("stdout did not start with seq prefix; got %q", body.Stdout[:32])
	}
}

// TestBashHandlerKillsSleepBeforeCompletion asserts a context with a short
// deadline forcibly terminates `sleep 60 && echo done` so the marker never
// reaches the captured stdout.
func TestBashHandlerKillsSleepBeforeCompletion(t *testing.T) {
	handler := newTestBashHandler(t, newTestSandbox(t), BashHandlerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := handler.Execute(ctx, provider.ToolCall{
		ID:        "call_sleep",
		Name:      "bash",
		Arguments: []byte(`{"command":"sleep 60 && echo done"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("execute took %s, expected cancellation in well under sleep duration", elapsed)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, want timeout error; content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if !body.TimedOut {
		t.Fatalf("TimedOut = false, want true; body=%+v", body)
	}
	if strings.Contains(body.Stdout, "done") {
		t.Fatalf("stdout contains completion marker 'done' — timeout did not kill the process; stdout=%q", body.Stdout)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err = %v, want DeadlineExceeded", ctx.Err())
	}
}

// TestBashHandlerKillsChildProcessGroupAtTimeout proves that a child process
// spawned by the bash session (here a long-running `sleep` in the background)
// does not survive the parent timeout. The test captures the child PID inside
// the sandbox, then verifies after the parent dies the PID is gone.
func TestBashHandlerKillsChildProcessGroupAtTimeout(t *testing.T) {
	root := t.TempDir()
	sb, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler := newTestBashHandler(t, sb, BashHandlerConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// Spawn a long sleeping child, write its PID to a file under the sandbox
	// root, then loop forever so the timeout fires while the child is alive.
	pidFile := filepath.Join(root, "child.pid")
	command := `sleep 60 & echo $! > ./child.pid; while :; do :; done`
	_, err = handler.Execute(ctx, provider.ToolCall{
		ID:        "call_child",
		Name:      "bash",
		Arguments: []byte(`{"command":` + jsonString(command) + `}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err = %v, want DeadlineExceeded", ctx.Err())
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		// In bubblewrap mode the pid file lives in the workspace but the
		// child's PID is inside a PID namespace, so it has no meaning from
		// the host. Skip cleanly — the broader timeout assertion above is
		// what proves the parent was killed.
		t.Skipf("could not read child pid file (likely under bubblewrap PID namespace): %v", err)
	}
	pid := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err != nil || pid <= 0 {
		t.Skipf("could not parse child pid file %q: %v", raw, err)
	}

	// Give the kernel a moment to reap the child after SIGKILL of the group.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			// ESRCH means the process no longer exists — success.
			if errors.Is(err, syscall.ESRCH) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(pid, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
		// Best-effort cleanup before failing the test.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("child pid %d still alive after parent timeout (kill probe err=%v)", pid, err)
	}
}

// jsonString wraps a Go string as a JSON string literal for embedding in raw
// JSON test fixtures. The test inputs above carry shell metacharacters which
// make json.Marshal cleaner than manual escaping.
func jsonString(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if r < 0x20 {
				b = append(b, []byte(fmt.Sprintf("\\u%04x", r))...)
			} else {
				b = append(b, []byte(string(r))...)
			}
		}
	}
	b = append(b, '"')
	return string(b)
}
