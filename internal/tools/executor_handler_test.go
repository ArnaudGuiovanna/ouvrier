package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

type handlerFunc func(context.Context, provider.ToolCall) (provider.ToolResult, error)

func (f handlerFunc) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	return f(ctx, call)
}

func TestExecutorRunsRegisteredHandler(t *testing.T) {
	executor := NewExecutor()
	err := executor.RegisterHandler("mcp_lookup", handlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		if call.Name != "mcp_lookup" {
			t.Fatalf("call name = %q, want mcp_lookup", call.Name)
		}
		content, _ := json.Marshal(map[string]string{"answer": "workers"})
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: content}, nil
	}), WithMetadata(Metadata{Effect: policy.EffectReadOnly}))
	if err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call_1", Name: "mcp_lookup"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	var decoded map[string]string
	if err := json.Unmarshal(result.Content, &decoded); err != nil {
		t.Fatalf("content is not JSON object: %v", err)
	}
	if decoded["answer"] != "workers" {
		t.Fatalf("answer = %q, want workers", decoded["answer"])
	}
}

func TestExecutorReturnsHandlerErrorAsToolResult(t *testing.T) {
	executor := NewExecutor()
	err := executor.RegisterHandler("mcp_lookup", handlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		return provider.ToolResult{}, context.DeadlineExceeded
	}), WithMetadata(Metadata{Effect: policy.EffectReadOnly}))
	if err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call_1", Name: "mcp_lookup"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want true")
	}
}

func TestExecutorValidatesRegisteredHandlerArguments(t *testing.T) {
	called := false
	executor := NewExecutor()
	err := executor.RegisterHandler("mcp_lookup", handlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		called = true
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: []byte(`"ok"`)}, nil
	}), WithMetadata(Metadata{
		Effect:      policy.EffectReadOnly,
		InputSchema: []byte(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
	}))
	if err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "mcp_lookup",
		Arguments: []byte(`{"query":7}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want schema validation error")
	}
	if called {
		t.Fatal("handler was called despite invalid arguments")
	}
	if !strings.Contains(string(result.Content), "validate tool arguments") {
		t.Fatalf("content = %s, want validation error", result.Content)
	}
}

func TestExecutorRequiresStateStoreForIdempotentHandler(t *testing.T) {
	called := false
	executor := NewExecutor()
	err := executor.RegisterHandler("publish", handlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		called = true
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: []byte(`"ok"`)}, nil
	}), WithMetadata(Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "ticket.id",
		InputSchema:    []byte(`{"type":"object","properties":{"ticket":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},"required":["ticket"]}`),
	}))
	if err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "publish",
		Arguments: []byte(`{"ticket":{"id":"T-1"}}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want missing StateStore idempotency error")
	}
	if called {
		t.Fatal("handler was called without idempotency StateStore")
	}
	if !strings.Contains(string(result.Content), "StateStore") {
		t.Fatalf("content = %s, want StateStore error", result.Content)
	}
}

func TestExecutorSkipsDuplicateIdempotentHandlerCall(t *testing.T) {
	store := state.NewMemoryStore()
	ctx := ContextWithIdempotencyStore(context.Background(), store, "exec_1")
	called := 0
	executor := NewExecutor()
	err := executor.RegisterHandler("publish", handlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		called++
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name, Content: []byte(`"ok"`)}, nil
	}), WithMetadata(Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "ticket.id",
		InputSchema:    []byte(`{"type":"object","properties":{"ticket":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},"required":["ticket"]}`),
	}))
	if err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "publish",
		Arguments: []byte(`{"ticket":{"id":"T-1"}}`),
	}

	first, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatalf("first Execute returned error: %v", err)
	}
	if first.IsError {
		t.Fatalf("first IsError = true, content=%s", first.Content)
	}
	second, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatalf("second Execute returned error: %v", err)
	}
	if !second.IsError {
		t.Fatal("second IsError = false, want duplicate idempotency error")
	}
	if called != 1 {
		t.Fatalf("called = %d, want exactly one handler execution", called)
	}
}
