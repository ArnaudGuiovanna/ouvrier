package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/provider"
)

func TestNewHTTPHandlerAppliesDefaultPolicyToTools(t *testing.T) {
	call := provider.ToolCall{
		ID:   "call_1",
		Name: "send_email",
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need approval", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "policy handled", StopReason: provider.StopEndTurn},
		},
	}
	called := false
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("notify owner",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("send_email", func(ctx context.Context) error {
				called = true
				return nil
			}, RequiresApproval()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if called {
		t.Fatal("approval-gated tool was called")
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Output != "policy handled" {
		t.Fatalf("output = %q, want policy handled", body.Output)
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(scripted.requests))
	}
	last := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1]
	if last.Role != provider.RoleTool || last.Blocks[0].ToolResult == nil {
		t.Fatalf("last message = %+v, want tool result", last)
	}
	result := last.Blocks[0].ToolResult
	if !result.IsError || !strings.Contains(string(result.Content), "permission denied") {
		t.Fatalf("tool result = %+v, want permission denial", result)
	}
}
