package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/provider"
	"ouvrier/internal/sandbox"
)

func TestBashHandlerRunsInWorkspaceWithAllowlistedEnv(t *testing.T) {
	root := t.TempDir()
	sandbox, err := sandbox.New(root,
		sandbox.WithEnvironment(map[string]string{
			"VISIBLE": "ok",
			"SECRET":  "hidden",
		}),
		sandbox.WithAllowedEnv("VISIBLE"),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler := newTestBashHandler(t, sandbox, BashHandlerConfig{})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf '%s' \"$VISIBLE:$SECRET:$PWD\" && pwd"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if !strings.Contains(body.Stdout, "ok::"+root) {
		t.Fatalf("stdout = %q, want visible env and sandbox PWD", body.Stdout)
	}
	if strings.Contains(body.Stdout, "hidden") {
		t.Fatalf("stdout leaked disallowed env: %q", body.Stdout)
	}
}

func TestNewBashHandlerUsesBubblewrapSandboxByDefaultWhenProbePasses(t *testing.T) {
	withFakeBashPlatform(t, nil)

	handler, err := NewBashHandler(newTestSandbox(t), BashHandlerConfig{})
	if err != nil {
		t.Fatalf("NewBashHandler returned error: %v", err)
	}
	if handler.mode != bashModeBubblewrap {
		t.Fatalf("mode = %v, want bubblewrap", handler.mode)
	}
}

func TestNewBashHandlerFailsFastWhenBubblewrapProbeFails(t *testing.T) {
	withFakeBashPlatform(t, errors.New("namespace disabled"))

	_, err := NewBashHandler(newTestSandbox(t), BashHandlerConfig{})
	if err == nil || !strings.Contains(err.Error(), "isolated bash sandbox unavailable") {
		t.Fatalf("NewBashHandler error = %v, want isolation fail-fast", err)
	}
}

func TestBubblewrapArgsDisableNetworkAndClearEnvironment(t *testing.T) {
	args := bubblewrapArgs("/usr/bin/bash", "/tmp/workspace", "/workspace/logs", "printf ok", map[string]string{
		"VISIBLE": "ok",
		"PWD":     "/workspace/logs",
	})

	if !containsArg(args, "--unshare-net") {
		t.Fatalf("args = %#v, want network namespace disabled", args)
	}
	if !containsArg(args, "--clearenv") {
		t.Fatalf("args = %#v, want cleared environment", args)
	}
	if !containsArgSequence(args, "--bind", "/tmp/workspace", "/workspace") {
		t.Fatalf("args = %#v, want workspace bind", args)
	}
	if !containsArgSequence(args, "--setenv", "VISIBLE", "ok") {
		t.Fatalf("args = %#v, want allowlisted environment", args)
	}
	if containsArg(args, "SECRET") {
		t.Fatalf("args = %#v, leaked non-allowlisted env key", args)
	}
}

func TestBashHandlerRejectsWorkdirEscape(t *testing.T) {
	handler := newTestBashHandler(t, newTestSandbox(t), BashHandlerConfig{})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_escape",
		Name:      "bash",
		Arguments: []byte(`{"command":"pwd","workdir":".."}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want workdir escape error")
	}
	body := decodeBashResult(t, result)
	if !strings.Contains(body.Error, "sandbox path escape") {
		t.Fatalf("error = %q, want sandbox path escape", body.Error)
	}
}

func TestBashHandlerTimesOutProcessGroup(t *testing.T) {
	handler := newTestBashHandler(t, newTestSandbox(t), BashHandlerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, err := handler.Execute(ctx, provider.ToolCall{
		ID:        "call_timeout",
		Name:      "bash",
		Arguments: []byte(`{"command":"while :; do :; done"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want timeout error")
	}
	body := decodeBashResult(t, result)
	if !body.TimedOut || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("result = %+v, ctx err = %v, want timed out deadline", body, ctx.Err())
	}
}

func TestBashHandlerBubblewrapHasNoDefaultNetworkRoute(t *testing.T) {
	if err := CheckBashIsolationAvailable(context.Background()); err != nil {
		t.Skipf("isolated bash sandbox unavailable on this host: %v", err)
	}
	handler, err := NewBashHandler(newTestSandbox(t), BashHandlerConfig{})
	if err != nil {
		t.Fatalf("NewBashHandler returned error: %v", err)
	}

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_network",
		Name:      "bash",
		Arguments: []byte(`{"command":"if grep -Eq '^[^[:space:]]+[[:space:]]+00000000[[:space:]]' /proc/net/route; then exit 7; fi; printf offline"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if body.Stdout != "offline" {
		t.Fatalf("stdout = %q, want offline marker", body.Stdout)
	}
}

func TestBashHandlerBubblewrapCannotReadHostFileThroughWorkspaceSymlink(t *testing.T) {
	if err := CheckBashIsolationAvailable(context.Background()); err != nil {
		t.Skipf("isolated bash sandbox unavailable on this host: %v", err)
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host-secret"), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sb, err := sandbox.New(root)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	handler, err := NewBashHandler(sb, BashHandlerConfig{})
	if err != nil {
		t.Fatalf("NewBashHandler returned error: %v", err)
	}

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_symlink",
		Name:      "bash",
		Arguments: []byte(`{"command":"cat outside/secret.txt"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, want symlink escape blocked; content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if strings.Contains(body.Stdout, "host-secret") || strings.Contains(body.Stderr, "host-secret") || strings.Contains(body.Error, "host-secret") {
		t.Fatalf("bash result leaked outside file: %+v", body)
	}
}

func TestBashHandlerBoundsCapturedOutput(t *testing.T) {
	handler := newTestBashHandler(t, newTestSandbox(t), BashHandlerConfig{MaxOutputBytes: 4})

	result, err := handler.Execute(context.Background(), provider.ToolCall{
		ID:        "call_output",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf 123456"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	body := decodeBashResult(t, result)
	if body.Stdout != "1234" || !body.Truncated || !body.StdoutTruncated || body.StderrTruncated {
		t.Fatalf("result = %+v, want bounded truncated stdout", body)
	}
}

func newTestSandbox(t *testing.T) *sandbox.Sandbox {
	t.Helper()

	sandbox, err := sandbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return sandbox
}

func newTestBashHandler(t *testing.T, sandbox *sandbox.Sandbox, cfg BashHandlerConfig) *BashHandler {
	t.Helper()

	cfg.AllowHostExecution = true
	handler, err := NewBashHandler(sandbox, cfg)
	if err != nil {
		t.Fatalf("NewBashHandler returned error: %v", err)
	}
	return handler
}

func withFakeBashPlatform(t *testing.T, probeErr error) {
	t.Helper()

	originalLookPath := bashLookPath
	originalProbe := probeBubblewrapSandbox
	t.Cleanup(func() {
		bashLookPath = originalLookPath
		probeBubblewrapSandbox = originalProbe
	})
	bashLookPath = func(file string) (string, error) {
		switch file {
		case "bash":
			return "/usr/bin/bash", nil
		case "bwrap":
			return "/usr/bin/bwrap", nil
		default:
			return "", errors.New("unexpected executable lookup")
		}
	}
	probeBubblewrapSandbox = func(ctx context.Context, bwrapPath, shellPath string, sandbox *sandbox.Sandbox) error {
		return probeErr
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsArgSequence(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		matched := true
		for j := range want {
			if args[i+j] != want[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func decodeBashResult(t *testing.T, result provider.ToolResult) BashResult {
	t.Helper()

	var body BashResult
	if err := json.Unmarshal(result.Content, &body); err != nil {
		t.Fatalf("tool content is not BashResult JSON: %v; content=%s", err, result.Content)
	}
	return body
}
