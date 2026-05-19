package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	"ouvrier/internal/state"
)

type lookupArgs struct {
	Query string `json:"query"`
}

type lookupResult struct {
	Answer string `json:"answer"`
}

func TestExecutorRunsTypedGoTool(t *testing.T) {
	executor := NewExecutor()
	err := executor.Register("lookup", func(ctx context.Context, args lookupArgs) (lookupResult, error) {
		if args.Query != "ouvrier" {
			t.Fatalf("query = %q, want ouvrier", args.Query)
		}
		return lookupResult{Answer: "workers"}, nil
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.ToolCallID != "call_1" || result.Name != "lookup" {
		t.Fatalf("ToolResult = %+v", result)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}

	var decoded lookupResult
	if err := json.Unmarshal(result.Content, &decoded); err != nil {
		t.Fatalf("result content is not lookupResult JSON: %v", err)
	}
	if decoded.Answer != "workers" {
		t.Fatalf("answer = %q, want workers", decoded.Answer)
	}
}

func TestExecutorRunsErrorOnlyGoTool(t *testing.T) {
	executor := NewExecutor()
	err := executor.Register("audit", func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call_1", Name: "audit"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}

	var decoded string
	if err := json.Unmarshal(result.Content, &decoded); err != nil {
		t.Fatalf("result content is not JSON string: %v", err)
	}
	if decoded != "ok" {
		t.Fatalf("content = %q, want ok", decoded)
	}
}

func TestExecutorReturnsToolErrorResult(t *testing.T) {
	executor := NewExecutor()
	boom := errors.New("lookup failed")
	err := executor.Register("lookup", func(ctx context.Context, args lookupArgs) (lookupResult, error) {
		return lookupResult{}, boom
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want true")
	}
	if !strings.Contains(string(result.Content), "lookup failed") {
		t.Fatalf("content = %s, want tool error", result.Content)
	}
}

func TestExecutorRejectsUnknownStructArgumentFields(t *testing.T) {
	executor := NewExecutor()
	called := false
	err := executor.Register("lookup", func(ctx context.Context, args lookupArgs) (lookupResult, error) {
		called = true
		return lookupResult{Answer: "workers"}, nil
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier","extra":true}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, want true; content=%s", result.Content)
	}
	if called {
		t.Fatal("tool was called despite unknown arguments")
	}
	if !strings.Contains(string(result.Content), "unknown field") {
		t.Fatalf("content = %s, want unknown field error", result.Content)
	}
}

func TestExecutorUnwrapsSingleValueObjectArgument(t *testing.T) {
	executor := NewExecutor()
	err := executor.Register("score", func(ctx context.Context, days int) (int, error) {
		return days + 1, nil
	}, WithMetadata(Metadata{ArgumentName: "days"}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "score",
		Arguments: []byte(`{"days":7}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	var decoded int
	if err := json.Unmarshal(result.Content, &decoded); err != nil {
		t.Fatalf("result content is not JSON number: %v", err)
	}
	if decoded != 8 {
		t.Fatalf("result = %d, want 8", decoded)
	}
}

func TestExecutorRejectsUnknownSingleValueObjectArguments(t *testing.T) {
	executor := NewExecutor()
	called := false
	err := executor.Register("score", func(ctx context.Context, days int) (int, error) {
		called = true
		return days, nil
	}, WithMetadata(Metadata{ArgumentName: "days"}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{
		ID:        "call_1",
		Name:      "score",
		Arguments: []byte(`{"days":7,"extra":true}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, want true; content=%s", result.Content)
	}
	if called {
		t.Fatal("tool was called despite unknown arguments")
	}
	if !strings.Contains(string(result.Content), "unknown field") {
		t.Fatalf("content = %s, want unknown field error", result.Content)
	}
}

func TestExecutorValidatesArgumentsAgainstInputSchema(t *testing.T) {
	inputSchema := json.RawMessage(`{
		"type":"object",
		"properties":{"days":{"type":"integer"}},
		"required":["days"],
		"additionalProperties":false
	}`)

	tests := []struct {
		name string
		args json.RawMessage
	}{
		{name: "missing required", args: []byte(`{}`)},
		{name: "null value", args: []byte(`{"days":null}`)},
		{name: "wrong type", args: []byte(`{"days":"seven"}`)},
		{name: "extra field", args: []byte(`{"days":7,"extra":true}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := NewExecutor()
			called := false
			err := executor.Register("score", func(ctx context.Context, days int) (int, error) {
				called = true
				return days, nil
			}, WithMetadata(Metadata{ArgumentName: "days", InputSchema: inputSchema}))
			if err != nil {
				t.Fatalf("Register returned error: %v", err)
			}

			result, err := executor.Execute(context.Background(), provider.ToolCall{
				ID:        "call_1",
				Name:      "score",
				Arguments: tt.args,
			})
			if err != nil {
				t.Fatalf("Execute returned error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("IsError = false, want true; content=%s", result.Content)
			}
			if called {
				t.Fatal("tool was called despite invalid schema arguments")
			}
			if !strings.Contains(string(result.Content), "validate tool arguments") {
				t.Fatalf("content = %s, want schema validation error", result.Content)
			}
		})
	}
}

func TestExecutorSkipsDuplicateIdempotentToolCall(t *testing.T) {
	store := state.NewMemoryStore()
	ctx := ContextWithIdempotencyStore(context.Background(), store, "exec_1")
	called := 0
	executor := NewExecutor()
	err := executor.Register("publish", func(ctx context.Context, args struct {
		Ticket struct {
			ID string `json:"id"`
		} `json:"ticket"`
	}) (string, error) {
		called++
		return args.Ticket.ID, nil
	}, WithMetadata(Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "ticket.id",
	}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
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
		t.Fatalf("second IsError = false, want duplicate idempotency error")
	}
	if called != 1 {
		t.Fatalf("called = %d, want exactly one tool execution", called)
	}
	if !strings.Contains(string(second.Content), "idempotency key") {
		t.Fatalf("second content = %s, want idempotency error", second.Content)
	}
}

func TestExecutorRejectsUnresolvableIdempotencyKey(t *testing.T) {
	store := state.NewMemoryStore()
	ctx := ContextWithIdempotencyStore(context.Background(), store, "exec_1")
	called := false
	executor := NewExecutor()
	err := executor.Register("publish", func(ctx context.Context, args lookupArgs) (lookupResult, error) {
		called = true
		return lookupResult{Answer: args.Query}, nil
	}, WithMetadata(Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "ticket.id",
	}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(ctx, provider.ToolCall{
		ID:        "call_1",
		Name:      "publish",
		Arguments: []byte(`{"query":"ouvrier"}`),
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want idempotency error result")
	}
	if called {
		t.Fatal("tool was called despite unresolvable idempotency key")
	}
	if !strings.Contains(string(result.Content), "resolve idempotency key") {
		t.Fatalf("content = %s, want key resolution error", result.Content)
	}
}

func TestExecutorRejectsUnsupportedSignature(t *testing.T) {
	executor := NewExecutor()
	err := executor.Register("bad", func(query string) error {
		return nil
	})
	if err == nil {
		t.Fatal("Register returned nil error, want signature validation")
	}
}

func TestExecutorHonorsCanceledContext(t *testing.T) {
	executor := NewExecutor()
	if err := executor.Register("audit", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "audit"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want context.Canceled", err)
	}
}

func TestExecutorOnlyMarksSubAgentHandlersAsParallel(t *testing.T) {
	executor := NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := executor.RegisterHandler("translate", handlerFunc(func(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
		return provider.ToolResult{ToolCallID: call.ID, Name: call.Name}, nil
	}), WithMetadata(Metadata{Kind: ToolKindSubAgent})); err != nil {
		t.Fatalf("RegisterHandler returned error: %v", err)
	}

	if executor.CanRunParallelSubAgent("lookup") {
		t.Fatal("lookup CanRunParallelSubAgent = true, want false")
	}
	if !executor.CanRunParallelSubAgent("translate") {
		t.Fatal("translate CanRunParallelSubAgent = false, want true")
	}
	if executor.CanRunParallelSubAgent("missing") {
		t.Fatal("missing CanRunParallelSubAgent = true, want false")
	}
}
