package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/mcpclient"
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
}

type httpMCPHandlerFunc func(context.Context, provider.ToolCall) (provider.ToolResult, error)

func (f httpMCPHandlerFunc) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	return f(ctx, call)
}

func (s *httpFakeMCPSession) RegisterTools(ctx context.Context, executor *tools.Executor) ([]provider.ToolSpec, error) {
	name := mcpclient.LocalToolName("moodle-mcp", "lookup")
	err := executor.RegisterHandler(name, httpMCPHandlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		content, _ := json.Marshal(map[string]string{"answer": "from mcp"})
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: content}, nil
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
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			MCP("moodle-mcp"),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, mcpConnector: connector})
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
}
