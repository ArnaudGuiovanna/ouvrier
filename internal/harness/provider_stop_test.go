package harness_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/schema"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

func TestRunTreatsProviderMaxTokensAsTruncated(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       `{"status":"partial"}`,
			StopReason: provider.StopMaxTokens,
			Usage:      provider.Usage{InputTokens: 3, OutputTokens: 5, CostUSD: 0.01},
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusTruncated {
		t.Fatalf("Status = %q, want truncated", out.Status)
	}
	if out.Text != `{"status":"partial"}` {
		t.Fatalf("Text = %q, want partial output preserved", out.Text)
	}
	if out.Iterations != 1 || len(p.requests) != 1 {
		t.Fatalf("Iterations = %d provider calls = %d, want 1 and 1", out.Iterations, len(p.requests))
	}
	if out.Usage.InputTokens != 3 || out.Usage.OutputTokens != 5 || out.Usage.CostUSD != 0.01 {
		t.Fatalf("Usage = %+v, want provider usage preserved", out.Usage)
	}

	execution, ok, err := store.Execution(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionTruncated || execution.CompletedAt.IsZero() {
		t.Fatalf("Execution = %+v, want truncated completion", execution)
	}

	event, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded event", stream.List())
	}
	if event.Payload["budget"] != "provider_max_tokens" ||
		event.Payload["stop_reason"] != string(provider.StopMaxTokens) ||
		event.Payload["output_tokens"] != 5 {
		t.Fatalf("budget event = %+v, want provider max-token details", event)
	}
}

func TestRunDoesNotReuseIntermediateTextWhenMaxTokensResponseIsEmpty(t *testing.T) {
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
			},
			{
				StopReason: provider.StopMaxTokens,
				Usage:      provider.Usage{InputTokens: 2, OutputTokens: 4},
			},
		},
	}
	executions := 0
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		executions++
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
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
	if out.Status != harness.StatusTruncated {
		t.Fatalf("Status = %q, want truncated", out.Status)
	}
	if out.Text != "" {
		t.Fatalf("Text = %q, want empty current response rather than intermediate text", out.Text)
	}
	if executions != 1 {
		t.Fatalf("tool executions = %d, want first-turn lookup only", executions)
	}
}

func TestRunDoesNotExecuteToolCallsFromMaxTokensResponse(t *testing.T) {
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "publish",
		Arguments: []byte(`{"value":"partial"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "partial",
			StopReason: provider.StopMaxTokens,
			ToolCalls:  []provider.ToolCall{call},
		}},
	}
	executions := 0
	executor := tools.NewExecutor(tools.WithPermissionPolicy(
		policy.NewDefaultPolicy(policy.AllowSideEffects("publish")),
	))
	if err := executor.Register("publish", func(ctx context.Context, args struct {
		Value string `json:"value"`
	}) (map[string]bool, error) {
		executions++
		return map[string]bool{"published": true}, nil
	}, tools.WithMetadata(tools.Metadata{
		Effect:      policy.EffectSideEffecting,
		SideEffects: []string{"publish"},
	})); err != nil {
		t.Fatalf("Register returned error: %v", err)
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
	if out.Status != harness.StatusTruncated {
		t.Fatalf("Status = %q, want truncated", out.Status)
	}
	if executions != 0 {
		t.Fatalf("tool executions = %d, want none for a truncated provider turn", executions)
	}
	if len(out.ToolCalls) != 0 {
		t.Fatalf("recorded tool calls = %d, want no accepted tool calls", len(out.ToolCalls))
	}
}

func TestRunPersistsMaxTokensAfterCancellationDuringBudgetEvent(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := stream.Subscribe(func(ctx context.Context, event events.Event) {
		if event.Kind == events.EventBudgetExceeded {
			cancel()
		}
	}); err != nil {
		t.Fatalf("Subscribe returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "partial",
			StopReason: provider.StopMaxTokens,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(runCtx, "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusTruncated {
		t.Fatalf("Status = %q, want truncated", out.Status)
	}
	execution, ok, err := store.Execution(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionTruncated || execution.CompletedAt.IsZero() {
		t.Fatalf("Execution = %+v, want persisted truncated completion", execution)
	}
}

func TestRunTreatsSchemaRepairMaxTokensAsTruncated(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	contract, err := schema.FromType(reflect.TypeFor[harnessSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{
				Text:       `{"status":1}`,
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 2, OutputTokens: 1},
			},
			{
				Text:       `{"status":"partial"}`,
				StopReason: provider.StopMaxTokens,
				Usage:      provider.Usage{InputTokens: 3, OutputTokens: 4},
			},
		},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
		harness.WithResultSchema(contract),
		harness.WithSchemaRepairAttempts(1),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Status != harness.StatusTruncated {
		t.Fatalf("Status = %q, want truncated", out.Status)
	}
	if out.Text != `{"status":"partial"}` {
		t.Fatalf("Text = %q, want repair response preserved", out.Text)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 5 {
		t.Fatalf("Usage = %+v, want initial plus truncated repair usage", out.Usage)
	}
	execution, ok, err := store.Execution(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok || execution.Status != state.ExecutionTruncated || execution.CompletedAt.IsZero() {
		t.Fatalf("Execution = %+v ok=%t, want truncated completion", execution, ok)
	}
	budgetEvent, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded event", stream.List())
	}
	if budgetEvent.Payload["budget"] != "provider_max_tokens" ||
		budgetEvent.Payload["output_tokens"] != 4 {
		t.Fatalf("budget event = %+v, want truncated repair details", budgetEvent)
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaRepairCompleted); ok {
		t.Fatalf("events = %+v, did not want schema repair completed", stream.List())
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaValidationPassed); ok {
		t.Fatalf("events = %+v, did not want schema validation passed", stream.List())
	}
}
