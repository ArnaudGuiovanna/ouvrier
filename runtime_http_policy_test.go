package ovr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	internalsandbox "ouvrier/internal/sandbox"
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

func TestNewHTTPHandlerDeniesWebhookPushByDefault(t *testing.T) {
	called := false
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called = true
	}))
	t.Cleanup(webhook.Close)
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if called {
		t.Fatal("webhook was called despite default output policy denial")
	}
	assertOutputPermissionDecision(t, stream, "push_webhook", false)
}

func TestNewHTTPHandlerAllowsExplicitWebhookPushPolicy(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{provider: scripted, eventStream: stream, toolExecutor: outputAllowedExecutor("webhook")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"status":"classified"}`)
	assertOutputPermissionDecision(t, stream, "push_webhook", true)
}

func TestNewHTTPHandlerRoutesWebhookPushThroughOutputToolPolicy(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	permissionPolicy := &outputToolOnlyPolicy{kind: policy.ActionPushWebhook}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{
		provider:    scripted,
		eventStream: stream,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			permissionPolicy,
		)),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"status":"classified"}`)
	if len(permissionPolicy.actions) != 1 {
		t.Fatalf("policy actions = %+v, want one output tool authorization", permissionPolicy.actions)
	}
	action := permissionPolicy.actions[0]
	if action.Kind != policy.ActionPushWebhook || action.ToolKind != string(tools.ToolKindOutput) || action.ToolName == "" {
		t.Fatalf("policy action = %+v, want registered output tool push_webhook action", action)
	}
	assertOutputPermissionDecision(t, stream, "push_webhook", true)
}

func TestNewHTTPHandlerDeniesQueuePushByDefault(t *testing.T) {
	queueURI, publishes := newNATSPublishRecorder(t, "tickets.classified")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Queue(queueURI)),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	select {
	case publish := <-publishes:
		t.Fatalf("queue was published despite default output policy denial: %+v", publish)
	default:
	}
	assertOutputPermissionDecision(t, stream, "push_queue", false)
}

func TestNewHTTPHandlerAllowsExplicitQueuePushPolicy(t *testing.T) {
	queueURI, publishes := newNATSPublishRecorder(t, "tickets.classified")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Queue(queueURI)),
	}, httpRuntime{provider: scripted, eventStream: stream, toolExecutor: outputAllowedExecutor("queue")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertNATSPublish(t, publishes)
	assertOutputPermissionDecision(t, stream, "push_queue", true)
}

func TestNewHTTPHandlerDeniesFileSinkByDefault(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "tickets.jsonl")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File(outputPath)),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	assertOutputPermissionDecision(t, stream, "sink_file", false)
}

func TestNewHTTPHandlerRejectsAllowedFileSinkOutsideSandbox(t *testing.T) {
	outputRoot := t.TempDir()
	outputPath := filepath.Join(t.TempDir(), "outside.json")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File(outputPath)),
	}, httpRuntime{
		provider:     scripted,
		eventStream:  stream,
		toolExecutor: outputAllowedExecutor("file"),
		sandbox:      fileSinkSandbox(t, outputRoot),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	assertOutputPermissionDecision(t, stream, "sink_file", true)
}

func TestRunnerSandboxConfiguresFileSinkBoundary(t *testing.T) {
	outputRoot := t.TempDir()
	outputPath := filepath.Join(outputRoot, "tickets.json")
	runner := NewRunner(
		WithPermissionPolicy(AllowSideEffectTargets("file", outputPath)),
		WithSandbox(Sandbox(outputRoot)),
	)
	rt := httpRuntime{provider: &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File(outputPath)),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
}

func TestRunnerSandboxAllowsSelectedEnvironmentOnly(t *testing.T) {
	t.Setenv("OVR_ALLOWED", "ok")
	t.Setenv("OVR_SECRET", "nope")
	runner := NewRunner(WithSandbox(Sandbox(t.TempDir(), AllowEnv("OVR_ALLOWED"))))
	rt := httpRuntime{}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}

	env := rt.sandbox.Environment()
	if env["OVR_ALLOWED"] != "ok" {
		t.Fatalf("OVR_ALLOWED = %q, want ok", env["OVR_ALLOWED"])
	}
	if _, leaked := env["OVR_SECRET"]; leaked {
		t.Fatalf("sandbox environment leaked OVR_SECRET: %+v", env)
	}
}

func TestNewHTTPHandlerAuditsLogSinkPermission(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"token":"secret"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertOutputPermissionDecision(t, stream, "sink_log", true)
	assertSinkLoggedEvent(t, stream, "input", `{"token":"[REDACTED]"}`)
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

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want parent request only after fatal subagent denial", len(scripted.requests))
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

func outputAllowedExecutor(labels ...string) *tools.Executor {
	options := []policy.PolicyOption{policy.AllowSideEffects(labels...)}
	for _, label := range labels {
		options = append(options, policy.AllowSideEffectTargets(label, "*"))
	}
	return tools.NewExecutor(tools.WithPermissionPolicy(
		policy.NewDefaultPolicy(options...),
	))
}

type outputToolOnlyPolicy struct {
	kind    policy.ActionKind
	actions []policy.Action
}

func (p *outputToolOnlyPolicy) Authorize(ctx context.Context, action policy.Action) (policy.Decision, error) {
	if err := ctx.Err(); err != nil {
		return policy.Decision{}, err
	}
	p.actions = append(p.actions, action)
	if action.Kind == p.kind && action.ToolKind == string(tools.ToolKindOutput) && strings.TrimSpace(action.ToolName) != "" {
		return policy.Allow("registered output tool action allowed"), nil
	}
	return policy.Deny("not a registered output tool action"), nil
}

func fileSinkSandbox(t *testing.T, root string) *internalsandbox.Sandbox {
	t.Helper()
	sandbox, err := internalsandbox.New(root)
	if err != nil {
		t.Fatalf("sandbox.New(%q) returned error: %v", root, err)
	}
	return sandbox
}

func assertOutputPermissionDecision(t *testing.T, stream *events.EventStream, action string, allowed bool) {
	t.Helper()
	for _, event := range stream.List() {
		if event.Kind != events.EventPermissionDecision {
			continue
		}
		if event.Payload["action"] == action && event.Payload["allowed"] == allowed {
			if _, leaked := event.Payload["target"]; leaked {
				t.Fatalf("permission event leaked output target: %+v", event.Payload)
			}
			if action != "sink_log" && strings.TrimSpace(fmt.Sprint(event.Payload["target_hash"])) == "" {
				t.Fatalf("permission event = %+v, want target_hash", event.Payload)
			}
			return
		}
	}
	t.Fatalf("events = %+v, want permission decision action=%s allowed=%v", stream.List(), action, allowed)
}
