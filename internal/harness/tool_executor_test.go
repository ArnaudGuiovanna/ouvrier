package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/harness"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	"ouvrier/internal/state"
	"ouvrier/internal/tools"
)

type harnessLookupArgs struct {
	Query string `json:"query"`
}

type harnessLookupResult struct {
	Answer string `json:"answer"`
}

func TestRunExecutesToolCallsThroughExecutor(t *testing.T) {
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need lookup", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	executor := tools.NewExecutor()
	err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		if args.Query != "ouvrier" {
			t.Fatalf("query = %q, want ouvrier", args.Query)
		}
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
		harness.WithTools(provider.ToolSpec{Name: "lookup", Description: "Lookup data."}),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.requests))
	}
	if len(p.requests[0].Tools) != 1 || p.requests[0].Tools[0].Name != "lookup" {
		t.Fatalf("provider tools = %+v, want lookup", p.requests[0].Tools)
	}

	second := p.requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Role != provider.RoleTool {
		t.Fatalf("last message role = %q, want tool", last.Role)
	}
	result := last.Blocks[0].ToolResult
	if result == nil {
		t.Fatal("tool result block is nil")
	}
	if result.IsError {
		t.Fatalf("tool result IsError = true, content=%s", result.Content)
	}
	var decoded harnessLookupResult
	if err := json.Unmarshal(result.Content, &decoded); err != nil {
		t.Fatalf("tool result content is not lookup JSON: %v", err)
	}
	if decoded.Answer != "workers" {
		t.Fatalf("tool answer = %q, want workers", decoded.Answer)
	}
}

func TestRunRetriesTransientReadOnlyToolError(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need lookup", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	called := 0
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		called++
		if called == 1 {
			return harnessLookupResult{}, provider.TransientError(errors.New("temporary lookup failure"))
		}
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(1),
		harness.WithToolExecutor(executor),
		harness.WithTools(provider.ToolSpec{Name: "lookup", Description: "Lookup data."}),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if called != 2 {
		t.Fatalf("called = %d, want one transient retry", called)
	}
	second := p.requests[1]
	last := second.Messages[len(second.Messages)-1]
	result := last.Blocks[0].ToolResult
	if result == nil || result.IsError {
		t.Fatalf("tool result = %+v, want successful retried result", result)
	}
	event, ok := findEvent(stream.List(), events.EventToolCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want retry tool_call_failed event", stream.List())
	}
	if event.Payload["tool"] != "lookup" ||
		event.Payload["tool_call_id"] != "call_1" ||
		event.Payload["attempt"] != 1 ||
		event.Payload["max_retries"] != 1 ||
		event.Payload["retrying"] != true ||
		event.Payload["transient"] != true {
		t.Fatalf("event payload = %+v, want retrying transient tool failure", event.Payload)
	}
}

type toolHandlerFunc func(context.Context, provider.ToolCall) (provider.ToolResult, error)

func (f toolHandlerFunc) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	return f(ctx, call)
}

func TestRunPassesSessionThroughToolContext(t *testing.T) {
	call := provider.ToolCall{ID: "call_1", Name: "inspect_session", Arguments: []byte(`{}`)}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need session", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	executor := tools.NewExecutor()
	if err := executor.RegisterHandler("inspect_session", toolHandlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		session, ok := harness.SessionFromContext(ctx)
		if !ok {
			t.Fatal("SessionFromContext ok = false, want true")
		}
		if session.ExecID == "" || session.SessionID == "" || session.TraceID == "" {
			t.Fatalf("session identifiers are empty: %+v", session)
		}
		content, _ := json.Marshal(map[string]string{"session_id": session.SessionID})
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: content}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
}

func TestRunEmitsPermissionDecisionForAllowedToolCallThroughEventStreamAndHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventPermissionDecision, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need lookup", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
		harness.WithEventStream(stream),
		harness.WithHookBus(hooks),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}

	event, ok := findEvent(stream.List(), events.EventPermissionDecision)
	if !ok {
		t.Fatalf("events = %+v, want permission decision event", stream.List())
	}
	if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["tool"] != "lookup" ||
		event.Payload["tool_call_id"] != "call_1" ||
		event.Payload["allowed"] != true ||
		event.Payload["effect"] != string(policy.EffectReadOnly) ||
		event.Payload["checked"] != true {
		t.Fatalf("event payload = %+v, want allowed lookup decision with hook enrichment", event.Payload)
	}
	if _, ok := event.Payload["arguments"]; ok {
		t.Fatalf("event payload = %+v, must not include tool arguments", event.Payload)
	}
}

func TestRunPersistsToolAndPermissionEventsToStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need lookup", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
		harness.WithStateStore(store),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	recorded, err := store.Events(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	for _, kind := range []events.EventKind{
		events.EventToolCallStarted,
		events.EventPermissionDecision,
		events.EventToolCallCompleted,
	} {
		event, ok := findEvent(recorded, kind)
		if !ok {
			t.Fatalf("persisted events = %+v, want %s", recorded, kind)
		}
		if event.Payload["tool_call_id"] != "call_1" {
			t.Fatalf("%s payload = %+v, want tool_call_id", kind, event.Payload)
		}
	}
}

func TestRunEmitsPermissionDecisionForDeniedToolCall(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{ID: "call_publish", Name: "publish", Arguments: []byte(`{"token":"secret"}`)}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need publish", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	called := false
	executor := tools.NewExecutor()
	if err := executor.Register("publish", func(ctx context.Context) error {
		called = true
		return nil
	}, tools.WithMetadata(tools.Metadata{
		Effect:           policy.EffectSideEffecting,
		SideEffects:      []string{"email"},
		RequiresApproval: true,
	})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if called {
		t.Fatal("tool function was called after permission denial")
	}

	event, ok := findEvent(stream.List(), events.EventPermissionDecision)
	if !ok {
		t.Fatalf("events = %+v, want permission decision event", stream.List())
	}
	if event.Payload["tool"] != "publish" ||
		event.Payload["tool_call_id"] != "call_publish" ||
		event.Payload["allowed"] != false ||
		event.Payload["requires_approval"] != true ||
		event.Payload["effect"] != string(policy.EffectSideEffecting) {
		t.Fatalf("event payload = %+v, want denied publish decision", event.Payload)
	}
	if event.Payload["reason"] == "" {
		t.Fatalf("event payload = %+v, want denial reason", event.Payload)
	}
	if _, ok := event.Payload["arguments"]; ok {
		t.Fatalf("event payload = %+v, must not include tool arguments", event.Payload)
	}

	last := p.requests[1].Messages[len(p.requests[1].Messages)-1]
	result := last.Blocks[0].ToolResult
	if result == nil || !result.IsError {
		t.Fatalf("tool result = %+v, want permission error result", result)
	}
}

func TestRunEmitsToolCallFailedForToolErrorResult(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{ID: "call_lookup", Name: "lookup", Arguments: []byte(`{"query":"ouvrier"}`)}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need lookup", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		return harnessLookupResult{}, errors.New("lookup unavailable")
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	event, ok := findEvent(stream.List(), events.EventToolCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want tool_call_failed", stream.List())
	}
	if event.Payload["tool_call_id"] != "call_lookup" {
		t.Fatalf("tool_call_failed payload = %+v, want tool_call_id", event.Payload)
	}
	if _, ok := findEvent(stream.List(), events.EventToolCallCompleted); ok {
		t.Fatalf("events = %+v, want no tool_call_completed for failed result", stream.List())
	}
}

func TestRunSkipsDuplicateIdempotentToolCall(t *testing.T) {
	store := state.NewMemoryStore()
	calls := []provider.ToolCall{
		{ID: "call_1", Name: "publish", Arguments: []byte(`{"ticket":{"id":"T-1"}}`)},
		{ID: "call_2", Name: "publish", Arguments: []byte(`{"ticket":{"id":"T-1"}}`)},
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need publish", StopReason: provider.StopToolUse, ToolCalls: calls},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}
	called := 0
	executor := tools.NewExecutor()
	if err := executor.Register("publish", func(ctx context.Context, args struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}) (string, error) {
		called++
		return args.Ticket.ID, nil
	}, tools.WithMetadata(tools.Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "ticket.id",
	})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
		harness.WithStateStore(store),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if called != 1 {
		t.Fatalf("called = %d, want exactly one side effect", called)
	}

	messages := p.requests[1].Messages
	firstResult := messages[len(messages)-2].Blocks[0].ToolResult
	secondResult := messages[len(messages)-1].Blocks[0].ToolResult
	if firstResult == nil || firstResult.IsError {
		t.Fatalf("first tool result = %+v, want success", firstResult)
	}
	if secondResult == nil || !secondResult.IsError {
		t.Fatalf("second tool result = %+v, want duplicate idempotency error", secondResult)
	}
	if !strings.Contains(string(secondResult.Content), "idempotency key") {
		t.Fatalf("second content = %s, want idempotency error", secondResult.Content)
	}
}

func TestRunExecutesParallelToolCallsConcurrentlyAndKeepsResultOrder(t *testing.T) {
	calls := []provider.ToolCall{
		{ID: "call_1", Name: "task", Arguments: []byte(`{"value":"first"}`)},
		{ID: "call_2", Name: "task", Arguments: []byte(`{"value":"second"}`)},
		{ID: "call_3", Name: "task", Arguments: []byte(`{"value":"third"}`)},
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "run tasks", StopReason: provider.StopToolUse, ToolCalls: calls},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
	}

	var mu sync.Mutex
	active := 0
	maxActive := 0
	started := make(chan struct{}, len(calls))
	release := make(chan struct{})

	executor := tools.NewExecutor()
	if err := executor.RegisterHandler("task", toolHandlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		started <- struct{}{}

		select {
		case <-release:
		case <-ctx.Done():
			return provider.ToolResult{}, ctx.Err()
		}

		mu.Lock()
		active--
		mu.Unlock()

		var args struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return provider.ToolResult{}, err
		}
		content, _ := json.Marshal(args.Value)
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: content}, nil
	}), tools.WithMetadata(tools.Metadata{Kind: tools.ToolKindSubAgent})); err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(executor),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		out, err := h.Run(context.Background(), "payload")
		if err == nil && out.Status != harness.StatusCompleted {
			err = context.Canceled
		}
		done <- err
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for parallel tool call %d", i+1)
		}
	}
	mu.Lock()
	observedParallel := maxActive
	mu.Unlock()
	if observedParallel < 2 {
		t.Fatalf("max active tool calls = %d, want at least 2", observedParallel)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish")
	}

	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.requests))
	}
	messages := p.requests[1].Messages
	if len(messages) < 4 {
		t.Fatalf("messages = %d, want assistant plus 3 tool results", len(messages))
	}
	got := make([]string, 0, len(calls))
	for _, message := range messages[len(messages)-len(calls):] {
		if message.Role != provider.RoleTool || len(message.Blocks) != 1 || message.Blocks[0].ToolResult == nil {
			t.Fatalf("message = %+v, want tool result", message)
		}
		var value string
		if err := json.Unmarshal(message.Blocks[0].ToolResult.Content, &value); err != nil {
			t.Fatalf("tool result content is not string: %v", err)
		}
		got = append(got, value)
	}
	want := []string{"first", "second", "third"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool result order = %+v, want %+v", got, want)
		}
	}
}
