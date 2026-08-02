package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPRuntimeDoesNotExposePreviousPipeTools(t *testing.T) {
	previousToolCalled := false
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: `{"status":"prepared"}`, StopReason: provider.StopEndTurn},
			{
				Text:       "need previous tool",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:   "call_previous",
					Name: "previous_pipe_tool",
				}},
			},
			{Text: `{"status":"isolated"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("prepare ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("previous_pipe_tool", func(context.Context) error {
				previousToolCalled = true
				return nil
			}, ReadOnly()),
		),
		Pipe("finish ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if previousToolCalled {
		t.Fatal("tool from previous Pipe was called by the following Pipe")
	}
	if len(scripted.requests) != 3 {
		t.Fatalf("provider calls = %d, want 3", len(scripted.requests))
	}
	result := lastHTTPToolResult(t, scripted.requests[2])
	if !result.IsError || !strings.Contains(string(result.Content), "tool not found") {
		t.Fatalf("tool result = %+v, want tool-not-found error", result)
	}
}

func TestHTTPRuntimeScopesReusedToolNamesToCurrentPipe(t *testing.T) {
	firstCalls := 0
	secondCalls := 0
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need first lookup",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:   "call_first",
					Name: "lookup",
				}},
			},
			{Text: `{"status":"prepared"}`, StopReason: provider.StopEndTurn},
			{
				Text:       "need second lookup",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:   "call_second",
					Name: "lookup",
				}},
			},
			{Text: `{"status":"isolated"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("prepare ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup", func(context.Context) (string, error) {
				firstCalls++
				return "first Pipe", nil
			}, ReadOnly()),
		),
		Pipe("finish ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup", func(context.Context) (string, error) {
				secondCalls++
				return "second Pipe", nil
			}, ReadOnly()),
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
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("tool calls = first:%d second:%d, want 1 each", firstCalls, secondCalls)
	}
	assertHTTPStringToolResult(t, scripted.requests[1], "first Pipe")
	assertHTTPStringToolResult(t, scripted.requests[3], "second Pipe")
}

func TestHTTPRuntimeScopesReusedIdempotentToolKeysToPipeDefinition(t *testing.T) {
	type publishArgs struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}
	arguments := json.RawMessage(`{"ticket":{"id":"T-1"}}`)
	firstCalls, secondCalls := 0, 0
	scripted := &httpScriptedProvider{responses: []provider.Response{
		{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "call_first", Name: "publish", Arguments: arguments}}},
		{Text: `{"status":"prepared"}`, StopReason: provider.StopEndTurn},
		{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "call_second", Name: "publish", Arguments: arguments}}},
		{Text: `{"status":"isolated"}`, StopReason: provider.StopEndTurn},
	}}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("prepare ticket", Model("anthropic/claude-sonnet-4-6"),
			Tool("publish", func(_ context.Context, args publishArgs) (string, error) {
				firstCalls++
				return "first:" + args.Ticket.ID, nil
			}, Idempotent("ticket.id"))),
		Pipe("finish ticket", Model("anthropic/claude-sonnet-4-6"),
			Tool("publish", func(_ context.Context, args publishArgs) (string, error) {
				secondCalls++
				return "second:" + args.Ticket.ID, nil
			}, Idempotent("ticket.id"))),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: state.NewMemoryStore()})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("idempotent tool calls = first:%d second:%d, want definition-isolated calls", firstCalls, secondCalls)
	}
	assertHTTPStringToolResult(t, scripted.requests[1], "first:T-1")
	assertHTTPStringToolResult(t, scripted.requests[3], "second:T-1")
}

func lastHTTPToolResult(t *testing.T, request provider.Request) *provider.ToolResult {
	t.Helper()
	if len(request.Messages) == 0 {
		t.Fatal("provider request has no messages")
	}
	message := request.Messages[len(request.Messages)-1]
	if message.Role != provider.RoleTool || len(message.Blocks) != 1 || message.Blocks[0].ToolResult == nil {
		t.Fatalf("last provider message = %+v, want one tool result", message)
	}
	return message.Blocks[0].ToolResult
}

func assertHTTPStringToolResult(t *testing.T, request provider.Request, want string) {
	t.Helper()
	result := lastHTTPToolResult(t, request)
	if result.IsError {
		t.Fatalf("tool result = %+v, want success", result)
	}
	var got string
	if err := json.Unmarshal(result.Content, &got); err != nil {
		t.Fatalf("tool result content = %q, want JSON string: %v", result.Content, err)
	}
	if got != want {
		t.Fatalf("tool result = %q, want %q", got, want)
	}
}
