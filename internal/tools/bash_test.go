package tools

import (
	"context"
	"encoding/json"
	"errors"
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

func TestNewBashHandlerRequiresUnsafeHostExecutionOptIn(t *testing.T) {
	_, err := NewBashHandler(newTestSandbox(t), BashHandlerConfig{})
	if err == nil || !strings.Contains(err.Error(), "cannot enforce filesystem, process, and network isolation") {
		t.Fatalf("NewBashHandler error = %v, want isolation fail-fast", err)
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

func decodeBashResult(t *testing.T, result provider.ToolResult) BashResult {
	t.Helper()

	var body BashResult
	if err := json.Unmarshal(result.Content, &body); err != nil {
		t.Fatalf("tool content is not BashResult JSON: %v; content=%s", err, result.Content)
	}
	return body
}
