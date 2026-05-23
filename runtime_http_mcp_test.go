package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/mcpclient"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	"ouvrier/internal/tools"
)

type httpFakeMCPConnector struct {
	session *httpFakeMCPSession
	server  string
}

func (c *httpFakeMCPConnector) Connect(ctx context.Context, serverName string) (mcpRuntimeSession, error) {
	c.server = serverName
	return c.session, nil
}

type httpFakeMCPSession struct {
	closed bool
	called bool
}

type httpMCPHandlerFunc func(context.Context, provider.ToolCall) (provider.ToolResult, error)

func (f httpMCPHandlerFunc) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	return f(ctx, call)
}

func (s *httpFakeMCPSession) RegisterTools(ctx context.Context, executor *tools.Executor) ([]provider.ToolSpec, error) {
	name := mcpclient.LocalToolName("moodle-mcp", "lookup")
	err := executor.RegisterHandler(name, httpMCPHandlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		s.called = true
		content, _ := json.Marshal(map[string]string{"answer": "from mcp"})
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: content}, nil
	}), tools.WithMetadata(tools.Metadata{
		Kind:        tools.ToolKindMCP,
		Target:      "moodle-mcp",
		Effect:      policy.EffectSideEffecting,
		SideEffects: []string{"mcp:moodle-mcp"},
	}))
	if err != nil {
		return nil, err
	}
	return []provider.ToolSpec{{Name: name, Description: "Lookup via MCP.", InputSchema: []byte(`{"type":"object"}`)}}, nil
}

func (s *httpFakeMCPSession) Close() error {
	s.closed = true
	return nil
}

func TestNewHTTPHandlerRegistersMCPToolsWithHarnessRuntime(t *testing.T) {
	toolName := mcpclient.LocalToolName("moodle-mcp", "lookup")
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:        "call_1",
					Name:      toolName,
					Arguments: []byte(`{"query":"ouvrier"}`),
				}},
			},
			{Text: `{"status":"done"}`, StopReason: provider.StopEndTurn},
		},
	}
	session := &httpFakeMCPSession{}
	connector := &httpFakeMCPConnector{session: session}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			MCP("moodle-mcp"),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:     scripted,
		mcpConnector: connector,
		eventStream:  stream,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffectTargets("mcp:moodle-mcp", "moodle-mcp")),
		)),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if connector.server != "moodle-mcp" {
		t.Fatalf("server = %q, want moodle-mcp", connector.server)
	}
	if !session.closed {
		t.Fatal("MCP session was not closed")
	}
	if !session.called {
		t.Fatal("MCP handler was not called")
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(scripted.requests))
	}
	if len(scripted.requests[0].Tools) != 1 || scripted.requests[0].Tools[0].Name != toolName {
		t.Fatalf("provider tools = %+v, want %s", scripted.requests[0].Tools, toolName)
	}
	last := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1]
	if last.Role != provider.RoleTool || last.Blocks[0].ToolResult == nil {
		t.Fatalf("last message = %+v, want tool result", last)
	}
	result := last.Blocks[0].ToolResult
	if result.Name != toolName || result.IsError {
		t.Fatalf("tool result = %+v", result)
	}
	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventPermissionDecision)
	if !ok {
		t.Fatalf("events = %+v, want MCP permission decision", stream.List())
	}
	if event.Payload["tool_kind"] != "mcp" || event.Payload["target"] != "moodle-mcp" {
		t.Fatalf("permission payload = %+v, want mcp target without endpoint/token", event.Payload)
	}
}

func TestNewHTTPHandlerDeniesMCPByDefaultBeforeRemoteCall(t *testing.T) {
	toolName := mcpclient.LocalToolName("moodle-mcp", "lookup")
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:        "call_1",
					Name:      toolName,
					Arguments: []byte(`{"query":"ouvrier"}`),
				}},
			},
			{Text: `{"status":"blocked"}`, StopReason: provider.StopEndTurn},
		},
	}
	session := &httpFakeMCPSession{}
	connector := &httpFakeMCPConnector{session: session}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			MCP("moodle-mcp"),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:     scripted,
		mcpConnector: connector,
		eventStream:  stream,
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if session.called {
		t.Fatal("MCP handler was called despite default policy denial")
	}
	if !session.closed {
		t.Fatal("MCP session was not closed")
	}
	toolResult := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || !toolResult.IsError || !strings.Contains(string(toolResult.Content), "side effect mcp:moodle-mcp target is not allowed") {
		t.Fatalf("tool result = %+v, want MCP policy denial", toolResult)
	}
	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventPermissionDecision)
	if !ok {
		t.Fatalf("events = %+v, want MCP permission decision", stream.List())
	}
	if event.Payload["tool_kind"] != "mcp" || event.Payload["target"] != "moodle-mcp" || event.Payload["allowed"] != false {
		t.Fatalf("permission payload = %+v, want denied mcp target", event.Payload)
	}
}
