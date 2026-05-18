package harness_test

import (
	"context"
	"encoding/json"
	"testing"

	"ouvrier/internal/harness"
	"ouvrier/internal/provider"
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
	})
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
