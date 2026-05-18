package tools

import (
	"context"
	"encoding/json"
	"testing"

	"ouvrier/internal/provider"
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
	}))
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
	}))
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
