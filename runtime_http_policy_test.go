package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	"ouvrier/internal/state"
	"ouvrier/internal/tools"
)

func TestNewHTTPHandlerAppliesDefaultPolicyToTools(t *testing.T) {
	call := provider.ToolCall{
		ID:   "call_1",
		Name: "send_email",
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need approval", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"policy handled"}`, StopReason: provider.StopEndTurn},
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
	if body.Output != `{"status":"policy handled"}` {
		t.Fatalf("output = %q, want policy handled JSON", body.Output)
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

func TestNewHTTPHandlerDeniesSideEffectingToolByDefault(t *testing.T) {
	call := provider.ToolCall{
		ID:   "call_email",
		Name: "send_email",
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need email", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"blocked"}`, StopReason: provider.StopEndTurn},
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
			}, SideEffecting("email")),
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
		t.Fatal("side-effecting tool was called without explicit allow policy")
	}
	result := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if result == nil || !result.IsError || !strings.Contains(string(result.Content), "side effect email is not allowed") {
		t.Fatalf("tool result = %+v, want denied email side effect", result)
	}
}

func TestNewHTTPHandlerAllowsExplicitSideEffectingTool(t *testing.T) {
	call := provider.ToolCall{
		ID:   "call_email",
		Name: "send_email",
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need email", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"sent"}`, StopReason: provider.StopEndTurn},
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
			}, SideEffecting("email")),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider: scripted,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffects("email")),
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
	if !called {
		t.Fatal("side-effecting tool was not called after explicit allow policy")
	}
	result := scripted.requests[1].Messages[len(scripted.requests[1].Messages)-1].Blocks[0].ToolResult
	if result == nil || result.IsError {
		t.Fatalf("tool result = %+v, want successful email side effect", result)
	}
}

func TestRunnerPermissionPolicyConfiguresHTTPRuntime(t *testing.T) {
	call := provider.ToolCall{
		ID:   "call_email",
		Name: "send_email",
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need email", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: `{"status":"sent"}`, StopReason: provider.StopEndTurn},
		},
	}
	called := false
	runner := NewRunner(WithPermissionPolicy(AllowSideEffects("email")))
	rt := httpRuntime{provider: scripted}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("notify owner",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("send_email", func(ctx context.Context) error {
				called = true
				return nil
			}, SideEffecting("email")),
		),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !called {
		t.Fatal("side-effecting tool was not called through Runner policy")
	}
}

func TestSubAgentCompositionCannotBypassPermissionPolicy(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	permissionPolicy := &denySubAgentPolicy{}
	runner := NewRunner(WithPermissionPolicy(permissionPolicy))
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need translation",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:        "call_translate",
					Name:      "translate",
					Arguments: []byte(`{"input":"hello"}`),
				}},
			},
			{
				Text:       `{"status":"blocked"}`,
				StopReason: provider.StopEndTurn,
			},
		},
	}
	rt := httpRuntime{provider: scripted, stateStore: store, eventStream: stream}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}
	translator := Pipeline(
		Pipe("translate text",
			Model("anthropic/claude-haiku-4-5"),
			Output[httpSubAgentReply](),
		),
	)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("translate", translator, MaxParallel(1)),
		),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/emails", strings.NewReader(`{"body":"hello"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want parent request and denial handling only", len(scripted.requests))
	}
	for _, req := range scripted.requests {
		if req.Model == "anthropic/claude-haiku-4-5" {
			t.Fatalf("child provider request ran despite policy denial: %+v", req)
		}
	}
	if !permissionPolicy.deniedSubAgent() {
		t.Fatalf("policy actions = %+v, want denied subagent action", permissionPolicy.actions)
	}

	sessions, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions returned error: %v", err)
	}
	var rootPipeSessionID string
	for _, session := range sessions {
		if session.ParentSessionID != "" {
			rootPipeSessionID = session.SessionID
			break
		}
	}
	if rootPipeSessionID == "" {
		t.Fatalf("sessions = %+v, want root pipe session", sessions)
	}
	for _, session := range sessions {
		if session.ParentSessionID == rootPipeSessionID {
			t.Fatalf("sessions = %+v, want no child subagent session after policy denial", sessions)
		}
	}

	var deniedPermission bool
	for _, event := range stream.List() {
		if event.Kind == events.EventTaskStarted && event.Payload["subagent"] == "translate" {
			t.Fatalf("task_started event emitted despite policy denial: %+v", event)
		}
		if event.Kind == events.EventPermissionDecision &&
			event.Payload["tool"] == "translate" &&
			event.Payload["tool_kind"] == "subagent" &&
			event.Payload["allowed"] == false {
			deniedPermission = true
		}
	}
	if !deniedPermission {
		t.Fatalf("events = %+v, want denied subagent permission decision", stream.List())
	}
}

type denySubAgentPolicy struct {
	actions []PermissionAction
}

func (p *denySubAgentPolicy) Authorize(ctx context.Context, action PermissionAction) (PermissionDecision, error) {
	if err := ctx.Err(); err != nil {
		return PermissionDecision{}, err
	}
	p.actions = append(p.actions, action)
	if action.ToolKind == "subagent" {
		return PermissionDecision{Allowed: false, Reason: "subagent blocked by policy"}, nil
	}
	return PermissionDecision{Allowed: true, Reason: "allowed by test policy"}, nil
}

func (p *denySubAgentPolicy) deniedSubAgent() bool {
	for _, action := range p.actions {
		if action.ToolName == "translate" && action.ToolKind == "subagent" {
			return true
		}
	}
	return false
}
