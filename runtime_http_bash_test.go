package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	"ouvrier/internal/tools"
)

func TestNewHTTPHandlerRunsBashThroughToolExecutorSandbox(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VISIBLE", "ok")
	t.Setenv("SECRET", "hidden")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_bash",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf '%s' \"$VISIBLE:$SECRET:$PWD\""}`),
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need shell", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"done"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root, AllowEnv("VISIBLE")), UnsafeBashHostExecution()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:     scripted,
		eventStream:  stream,
		toolExecutor: bashAllowedExecutor(),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/logs", strings.NewReader(`{"file":"app.log"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial call and final call", len(scripted.requests))
	}
	if !hasToolSpec(scripted.requests[0].Tools, "bash") {
		t.Fatalf("tools = %+v, want bash tool spec", scripted.requests[0].Tools)
	}

	toolResult := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || toolResult.IsError {
		t.Fatalf("tool result = %+v, want successful bash result", toolResult)
	}
	var bashResult runtimeHTTPBashResult
	if err := json.Unmarshal(toolResult.Content, &bashResult); err != nil {
		t.Fatalf("bash result content is not JSON: %v; content=%s", err, toolResult.Content)
	}
	if !strings.Contains(bashResult.Stdout, "ok::"+root) {
		t.Fatalf("stdout = %q, want allowlisted env and sandbox PWD", bashResult.Stdout)
	}
	if strings.Contains(bashResult.Stdout, "hidden") {
		t.Fatalf("stdout leaked secret env: %q", bashResult.Stdout)
	}

	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventPermissionDecision)
	if !ok {
		t.Fatalf("events = %+v, want permission decision", stream.List())
	}
	if event.Payload["tool_kind"] != "bash" || event.Payload["allowed"] != true || event.Payload["target"] == "" {
		t.Fatalf("permission event = %+v, want allowed bash target", event.Payload)
	}
}

func TestNewHTTPHandlerRunsIsolatedBashByDefaultWhenAvailable(t *testing.T) {
	if err := checkBashIsolationAvailable(context.Background()); err != nil {
		t.Skipf("isolated bash sandbox unavailable on this host: %v", err)
	}
	root := t.TempDir()
	t.Setenv("VISIBLE", "ok")
	t.Setenv("SECRET", "hidden")
	call := provider.ToolCall{
		ID:        "call_bash",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf '%s' \"$VISIBLE:$SECRET:$PWD\""}`),
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need shell", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"done"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root, AllowEnv("VISIBLE"))),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:     scripted,
		toolExecutor: bashAllowedExecutor(),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/logs", strings.NewReader(`{"file":"app.log"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	toolResult := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || toolResult.IsError {
		t.Fatalf("tool result = %+v, want successful isolated bash result", toolResult)
	}
	var bashResult runtimeHTTPBashResult
	if err := json.Unmarshal(toolResult.Content, &bashResult); err != nil {
		t.Fatalf("bash result content is not JSON: %v; content=%s", err, toolResult.Content)
	}
	if !strings.Contains(bashResult.Stdout, "ok::/workspace") {
		t.Fatalf("stdout = %q, want allowlisted env and container workspace PWD", bashResult.Stdout)
	}
	if strings.Contains(bashResult.Stdout, "hidden") || strings.Contains(bashResult.Stdout, root) {
		t.Fatalf("stdout leaked host details: %q", bashResult.Stdout)
	}
}

func TestNewHTTPHandlerDeniesBashByDefault(t *testing.T) {
	root := t.TempDir()
	call := provider.ToolCall{
		ID:        "call_bash",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf ok"}`),
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need shell", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"blocked"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root), UnsafeBashHostExecution()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/logs", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	toolResult := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || !toolResult.IsError || !strings.Contains(string(toolResult.Content), "side effect process target is not allowed") {
		t.Fatalf("tool result = %+v, want default policy denial", toolResult)
	}
}

func TestRuntimeRejectsBashWhenIsolatedBackendUnavailable(t *testing.T) {
	originalCheck := checkBashIsolationAvailable
	t.Cleanup(func() {
		checkBashIsolationAvailable = originalCheck
	})
	checkBashIsolationAvailable = func(context.Context) error {
		return errors.New("namespace disabled")
	}

	root := t.TempDir()
	nodes := []Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root)),
		),
		Reply(JSON[httpTestReply]()),
	}

	_, err := newHTTPHandlerWithRuntime(nodes, httpRuntime{
		provider:     &httpScriptedProvider{},
		toolExecutor: bashAllowedExecutor(),
	})
	if err == nil || !strings.Contains(err.Error(), "isolated Bash sandbox unavailable") {
		t.Fatalf("newHTTPHandlerWithRuntime error = %v, want bash isolation fail-fast", err)
	}
}

func TestNewHTTPHandlerBashRejectsSandboxWorkdirEscape(t *testing.T) {
	root := t.TempDir()
	call := provider.ToolCall{
		ID:        "call_bash",
		Name:      "bash",
		Arguments: []byte(`{"command":"pwd","workdir":".."}`),
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need shell", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"blocked"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root), UnsafeBashHostExecution()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, toolExecutor: bashAllowedExecutor()})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/logs", strings.NewReader(`{"file":"app.log"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	toolResult := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || !toolResult.IsError {
		t.Fatalf("tool result = %+v, want bash workdir error", toolResult)
	}
	var bashResult runtimeHTTPBashResult
	if err := json.Unmarshal(toolResult.Content, &bashResult); err != nil {
		t.Fatalf("bash result content is not JSON: %v; content=%s", err, toolResult.Content)
	}
	if !strings.Contains(bashResult.Error, "sandbox path escape") {
		t.Fatalf("bash error = %q, want sandbox path escape", bashResult.Error)
	}
}

func TestNewHTTPHandlerBashTruncationIsObservable(t *testing.T) {
	root := t.TempDir()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_bash",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf 123456"}`),
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need shell", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"done"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root), UnsafeBashHostExecution(), BashMaxOutputBytes(4)),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:     scripted,
		eventStream:  stream,
		toolExecutor: bashAllowedExecutor(),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/logs", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventToolCallCompleted)
	if !ok {
		t.Fatalf("events = %+v, want completed bash tool event", stream.List())
	}
	if event.Payload["output_truncated"] != true || event.Payload["stdout_truncated"] != true {
		t.Fatalf("tool event payload = %+v, want truncation markers", event.Payload)
	}
}

func TestRunnerBashUsesConfiguredPermissionPolicy(t *testing.T) {
	root := t.TempDir()
	call := provider.ToolCall{
		ID:        "call_bash",
		Name:      "bash",
		Arguments: []byte(`{"command":"printf ok"}`),
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need shell", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"blocked"}`, StopReason: provider.StopEndTurn},
		},
	}
	policy := denyBashPolicy{}
	runner := NewRunner(WithPermissionPolicy(policy))
	rt := httpRuntime{provider: scripted}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /logs"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root), UnsafeBashHostExecution()),
		),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/logs", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	toolResult := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || !toolResult.IsError || !strings.Contains(string(toolResult.Content), "bash denied") {
		t.Fatalf("tool result = %+v, want policy denial", toolResult)
	}
}

type runtimeHTTPBashResult struct {
	Stdout string `json:"stdout"`
	Error  string `json:"error,omitempty"`
}

type denyBashPolicy struct{}

func (denyBashPolicy) Authorize(ctx context.Context, action PermissionAction) (PermissionDecision, error) {
	if action.ToolKind == "bash" {
		return PermissionDecision{Allowed: false, Reason: "bash denied"}, nil
	}
	return AllowSideEffectTargets("process", "*").Authorize(ctx, action)
}

func hasToolSpec(specs []provider.ToolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name && len(spec.InputSchema) > 0 {
			return true
		}
	}
	return false
}

func bashAllowedExecutor() *tools.Executor {
	return tools.NewExecutor(tools.WithPermissionPolicy(
		policy.NewDefaultPolicy(
			policy.AllowSideEffectTargets("process", "*"),
			policy.AllowSideEffectTargets("filesystem", "*"),
		),
	))
}
