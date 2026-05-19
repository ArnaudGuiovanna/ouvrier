package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ouvrier/internal/provider"
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
