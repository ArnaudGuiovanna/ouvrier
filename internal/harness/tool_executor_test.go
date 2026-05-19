package harness_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

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
