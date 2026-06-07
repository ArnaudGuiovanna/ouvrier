package harness_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/schema"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

type scriptedProvider struct {
	responses []provider.Response
	err       error
	errors    []error
	requests  []provider.Request
}

func (p *scriptedProvider) Name() string {
	return "scripted"
}

func (p *scriptedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return provider.Response{}, p.err
	}
	idx := len(p.requests) - 1
	if idx < len(p.errors) && p.errors[idx] != nil {
		return provider.Response{}, p.errors[idx]
	}
	if len(p.responses) == 0 {
		return provider.Response{}, nil
	}
	if idx >= len(p.responses) {
		idx = len(p.responses) - 1
	}
	return p.responses[idx], nil
}

func TestRunCompletesWithProviderText(t *testing.T) {
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 3, OutputTokens: 5, CostUSD: 0.01},
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithSystemPrompt("You are concise."),
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
	if out.Text != "done" {
		t.Fatalf("Text = %q, want done", out.Text)
	}
	if out.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", out.Iterations)
	}
	if out.Session.SessionID == "" || out.Session.ExecID == "" || out.Session.TraceID == "" {
		t.Fatalf("Session IDs were not initialized: %+v", out.Session)
	}
	if out.Session.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("Session model = %q", out.Session.Model)
	}
	if out.Session.Budget.MaxIterations != harness.DefaultMaxIterations {
		t.Fatalf("Session max iterations = %d", out.Session.Budget.MaxIterations)
	}
	if out.Usage.InputTokens != 3 || out.Usage.OutputTokens != 5 || out.Usage.CostUSD != 0.01 {
		t.Fatalf("Usage = %+v, want aggregated response usage", out.Usage)
	}

	if len(p.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(p.requests))
	}
	req := p.requests[0]
	if req.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("request model = %q", req.Model)
	}
	if req.System != "You are concise." {
		t.Fatalf("request system = %q", req.System)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("request messages = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != provider.RoleUser {
		t.Fatalf("first message role = %q, want user", req.Messages[0].Role)
	}
	if req.Messages[0].Text() != "payload" {
		t.Fatalf("first message text = %q, want payload", req.Messages[0].Text())
	}
}

func TestRunStopsAtIterationBudget(t *testing.T) {
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"x"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need lookup", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "need lookup again", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "should not be reached", StopReason: provider.StopEndTurn},
		},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithMaxIterations(2),
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
	if out.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2", out.Iterations)
	}
	if len(out.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(out.ToolCalls))
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.requests))
	}
}

func TestRunStoresConfiguredTokenAndCostBudgetOnSession(t *testing.T) {
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 3, OutputTokens: 4, CostUSD: 0.05},
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithBudget(runtimecore.Budget{
			MaxIterations: 7,
			MaxTokens:     40,
			MaxCostUSD:    0.50,
			MaxWallClock:  time.Minute,
		}),
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
	wantBudget := runtimecore.Budget{MaxIterations: 7, MaxTokens: 40, MaxCostUSD: 0.50, MaxWallClock: time.Minute}
	if out.Session.Budget != wantBudget {
		t.Fatalf("Session budget = %+v, want %+v", out.Session.Budget, wantBudget)
	}
}

func TestRunStopsWhenWallClockBudgetExceeded(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	p := &blockingHarnessProvider{started: make(chan struct{})}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithBudget(runtimecore.Budget{
			MaxIterations: 5,
			MaxTokens:     100,
			MaxCostUSD:    1,
			MaxWallClock:  10 * time.Millisecond,
		}),
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
	select {
	case <-p.started:
	default:
		t.Fatal("provider was not called")
	}

	event, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded event", stream.List())
	}
	if event.Payload["budget"] != "wallclock" || event.Payload["max_wallclock_ms"] != int64(10) {
		t.Fatalf("budget event = %+v, want wallclock details", event)
	}
}

func TestRunStopsBeforeNextLLMWhenTokenBudgetExceeded(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"x"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
				Usage:      provider.Usage{InputTokens: 4, OutputTokens: 1, CostUSD: 0.05},
			},
			{
				Text:       "over budget",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
				Usage:      provider.Usage{InputTokens: 5, OutputTokens: 1, CostUSD: 0.04},
			},
			{
				Text:       "should not run",
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1, CostUSD: 0.01},
			},
		},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithBudget(runtimecore.Budget{MaxIterations: 5, MaxTokens: 10, MaxCostUSD: 1.00}),
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
	if out.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2", out.Iterations)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.requests))
	}
	if out.Usage.InputTokens != 9 || out.Usage.OutputTokens != 2 || out.Usage.CostUSD != 0.09 {
		t.Fatalf("Usage = %+v, want aggregate usage across completed LLM calls", out.Usage)
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
	if event.Payload["budget"] != "tokens" ||
		event.Payload["max_tokens"] != 10 ||
		event.Payload["used_tokens"] != 11 ||
		event.ExecID != out.Session.ExecID ||
		event.SessionID != out.Session.SessionID {
		t.Fatalf("budget event = %+v, want token budget details and session identifiers", event)
	}
}

func TestRunStopsBeforeNextLLMWhenCostBudgetExceeded(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"x"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
				Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1, CostUSD: 0.20},
			},
			{
				Text:       "over budget",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
				Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1, CostUSD: 0.31},
			},
			{
				Text:       "should not run",
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1, CostUSD: 0.01},
			},
		},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithBudget(runtimecore.Budget{MaxIterations: 5, MaxTokens: 100, MaxCostUSD: 0.50}),
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
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(p.requests))
	}

	event, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded event", stream.List())
	}
	if event.Payload["budget"] != "cost_usd" ||
		event.Payload["max_cost_usd"] != 0.50 ||
		event.Payload["used_cost_usd"] != 0.51 {
		t.Fatalf("budget event = %+v, want cost budget details", event)
	}
}

func TestRunReturnsFailedOutcomeOnProviderError(t *testing.T) {
	boom := errors.New("provider exploded")
	p := &scriptedProvider{err: boom}
	h, err := harness.New(p, harness.WithModel("anthropic/claude-sonnet-4-6"))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want provider error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if out.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1 attempted provider call", out.Iterations)
	}
}

func TestRunEmitsProviderFailureMetadataForTrace(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	authErr := provider.AuthError(errors.New("invalid api key"))
	p := &scriptedProvider{err: authErr}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(2),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, authErr) {
		t.Fatalf("Run error = %v, want auth error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}

	event, ok := findEvent(stream.List(), events.EventLLMCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want LLM failed event", stream.List())
	}
	if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["provider"] != "scripted" ||
		event.Payload["error_kind"] != string(provider.ErrorAuth) ||
		event.Payload["model"] != "anthropic/claude-sonnet-4-6" ||
		event.Payload["attempt"] != 1 ||
		event.Payload["retrying"] != false ||
		event.Payload["transient"] != false {
		t.Fatalf("event payload = %+v, want provider failure metadata for trace/admin", event.Payload)
	}
}

func TestRunRetriesTransientProviderErrorBeforeSideEffects(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventLLMCallFailed, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	p := &scriptedProvider{
		errors: []error{provider.TransientError(errors.New("rate limited"))},
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(2),
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
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial call plus retry", len(p.requests))
	}
	if out.Iterations != 1 {
		t.Fatalf("Iterations = %d, want one logical LLM iteration", out.Iterations)
	}
	event, ok := findEvent(stream.List(), events.EventLLMCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want LLM failed event for transient retry", stream.List())
	}
	if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["iteration"] != 1 ||
		event.Payload["attempt"] != 1 ||
		event.Payload["retrying"] != true ||
		event.Payload["transient"] != true ||
		event.Payload["checked"] != true {
		t.Fatalf("event payload = %+v, want retrying transient failure with hook enrichment", event.Payload)
	}
	if _, ok := event.Payload["messages"]; ok {
		t.Fatalf("event payload = %+v, must not include request messages", event.Payload)
	}
}

func TestRunDoesNotRetryAuthProviderError(t *testing.T) {
	authErr := provider.AuthError(errors.New("invalid api key"))
	p := &scriptedProvider{err: authErr}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(3),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, authErr) {
		t.Fatalf("Run error = %v, want auth error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if len(p.requests) != 1 {
		t.Fatalf("provider calls = %d, want no retry for auth error", len(p.requests))
	}
}

func TestRunDoesNotRetryTransientProviderErrorAfterToolCall(t *testing.T) {
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
		responses: []provider.Response{{
			Text:       "need lookup",
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{call},
		}},
		errors: []error{nil, provider.TransientError(errors.New("network reset"))},
	}
	executor := tools.NewExecutor(tools.WithPermissionPolicy(
		policy.NewDefaultPolicy(policy.AllowSideEffects("lookup")),
	))
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{
		Effect:      policy.EffectSideEffecting,
		SideEffects: []string{"lookup"},
	})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(2),
		harness.WithToolExecutor(executor),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err == nil {
		t.Fatal("Run returned nil, want provider error after tool call")
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want no retry after tool call", len(p.requests))
	}
	event, ok := findEvent(stream.List(), events.EventLLMCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want LLM failed event after tool call", stream.List())
	}
	if event.Payload["iteration"] != 2 ||
		event.Payload["attempt"] != 1 ||
		event.Payload["retrying"] != false ||
		event.Payload["transient"] != true {
		t.Fatalf("event payload = %+v, want non-retry transient failure after tool call", event.Payload)
	}
}

func TestRunRetriesTransientProviderErrorAfterReadOnlyToolCall(t *testing.T) {
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
		errors: []error{nil, provider.TransientError(errors.New("network reset"))},
	}
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(2),
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
	if len(p.requests) != 3 {
		t.Fatalf("provider calls = %d, want retry after read-only tool", len(p.requests))
	}
	event, ok := findEvent(stream.List(), events.EventLLMCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want LLM failed event for transient retry", stream.List())
	}
	if event.Payload["iteration"] != 2 ||
		event.Payload["attempt"] != 1 ||
		event.Payload["retrying"] != true ||
		event.Payload["transient"] != true {
		t.Fatalf("event payload = %+v, want retrying transient failure after read-only tool", event.Payload)
	}
}

func TestRunRetriesTransientProviderErrorAfterIdempotentToolCall(t *testing.T) {
	store := state.NewMemoryStore()
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "publish",
		Arguments: []byte(`{"ticket":{"id":"T-1"}}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: "need publish", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{Text: "done", StopReason: provider.StopEndTurn},
		},
		errors: []error{nil, provider.TransientError(errors.New("network reset"))},
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
		harness.WithProviderRetries(2),
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
	if len(p.requests) != 3 {
		t.Fatalf("provider calls = %d, want retry after idempotent tool", len(p.requests))
	}
	if called != 1 {
		t.Fatalf("called = %d, want one idempotent tool execution", called)
	}
}

func TestRunPersistsSessionAndExecutionWhenStateStoreConfigured(t *testing.T) {
	store := state.NewMemoryStore()
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	execution, ok, err := store.Execution(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionCompleted || execution.CompletedAt.IsZero() {
		t.Fatalf("Execution = %+v", execution)
	}

	session, ok, err := store.Session(context.Background(), out.Session.SessionID)
	if err != nil {
		t.Fatalf("Session returned error: %v", err)
	}
	if !ok {
		t.Fatal("Session ok = false, want true")
	}
	if session.ExecID != out.Session.ExecID || session.TraceID != out.Session.TraceID {
		t.Fatalf("Session = %+v, want trace for %+v", session, out.Session)
	}
}

func TestRunPersistsEventsToStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	recorded, err := store.Events(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(recorded) == 0 {
		t.Fatal("events = 0, want persisted harness events")
	}
	var started, completed bool
	for _, event := range recorded {
		if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
			t.Fatalf("event = %+v, want session identifiers", event)
		}
		if event.Kind == events.EventSessionStarted {
			started = true
		}
		if event.Kind == events.EventPipeCompleted {
			completed = true
		}
	}
	if !started || !completed {
		t.Fatalf("events = %+v, want session start and pipe completed", recorded)
	}
}

func TestRunWithParentSessionCreatesChildWithoutFinishingExecution(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	parent, err := runtimecore.NewSession("anthropic/claude-sonnet-4-6",
		runtimecore.WithSessionIDs("exec_parent", "sess_parent", "trace_parent"),
		runtimecore.WithSessionBudget(runtimecore.Budget{
			MaxIterations: 9,
			MaxTokens:     90,
			MaxCostUSD:    0.90,
			MaxWallClock:  time.Minute,
		}),
	)
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if err := store.SaveExecution(context.Background(), state.Execution{
		ExecID:    parent.ExecID,
		TraceID:   parent.TraceID,
		Status:    state.ExecutionRunning,
		StartedAt: parent.StartedAt,
	}); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}

	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "child done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-haiku-4-5"),
		harness.WithParentSession(parent),
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
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if out.Session.ExecID != parent.ExecID || out.Session.TraceID != parent.TraceID {
		t.Fatalf("child session = %+v, want parent exec/trace", out.Session)
	}
	if out.Session.ParentSessionID != parent.SessionID {
		t.Fatalf("ParentSessionID = %q, want %q", out.Session.ParentSessionID, parent.SessionID)
	}
	if out.Session.Budget != parent.Budget {
		t.Fatalf("child budget = %+v, want inherited %+v", out.Session.Budget, parent.Budget)
	}

	execution, ok, err := store.Execution(context.Background(), parent.ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionRunning || !execution.CompletedAt.IsZero() {
		t.Fatalf("execution = %+v, want child run to leave root execution running", execution)
	}

	child, ok, err := store.Session(context.Background(), out.Session.SessionID)
	if err != nil {
		t.Fatalf("Session returned error: %v", err)
	}
	if !ok {
		t.Fatal("Session ok = false, want stored child session")
	}
	if child.ParentSessionID != parent.SessionID {
		t.Fatalf("stored child = %+v, want parent lineage", child)
	}
	event, ok := findEvent(stream.List(), events.EventSessionStart)
	if !ok {
		t.Fatalf("events = %+v, want child session start", stream.List())
	}
	if event.ExecID != parent.ExecID || event.TraceID != parent.TraceID || event.SessionID != out.Session.SessionID {
		t.Fatalf("event = %+v, want child session identifiers", event)
	}
}

func TestRunMarksExecutionFailedOnProviderError(t *testing.T) {
	store := state.NewMemoryStore()
	boom := errors.New("provider exploded")
	p := &scriptedProvider{err: boom}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want provider error", err)
	}

	execution, ok, err := store.Execution(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionFailed || execution.CompletedAt.IsZero() {
		t.Fatalf("Execution = %+v", execution)
	}
}

func TestRunValidatesResultSchemaAndRecordsViolation(t *testing.T) {
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
		responses: []provider.Response{{
			Text:       `{"status":1}`,
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
		harness.WithResultSchema(contract),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err == nil {
		t.Fatal("Run returned nil error for invalid schema output")
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}

	violations, err := store.SchemaViolations(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(violations))
	}
	if violations[0].SessionID != out.Session.SessionID || violations[0].SchemaName != contract.Name {
		t.Fatalf("violation = %+v, want session and schema name", violations[0])
	}

	foundViolationEvent := false
	for _, event := range stream.List() {
		if event.Kind == events.EventSchemaViolation {
			foundViolationEvent = true
			if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID {
				t.Fatalf("event = %+v, want session identifiers", event)
			}
		}
	}
	if !foundViolationEvent {
		t.Fatalf("events = %+v, want schema violation event", stream.List())
	}

	persistedEvents, err := store.Events(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if _, ok := findEvent(persistedEvents, events.EventSchemaViolation); !ok {
		t.Fatalf("persisted events = %+v, want schema violation event", persistedEvents)
	}
	failed, ok := findEvent(persistedEvents, events.EventSchemaRepairFailed)
	if !ok {
		t.Fatalf("persisted events = %+v, want schema repair failed event", persistedEvents)
	}
	if failed.Payload["reason"] != "disabled" || failed.Payload["max_attempts"] != 0 {
		t.Fatalf("schema repair failed payload = %+v, want disabled reason with zero attempts", failed.Payload)
	}
}

func TestRunNormalizesFencedResultSchemaOutput(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	contract, err := schema.FromType(reflect.TypeFor[harnessSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "```json\n{\"status\":\"ok\"}\n```",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithResultSchema(contract),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Text != `{"status":"ok"}` {
		t.Fatalf("Text = %q, want normalized JSON", out.Text)
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaViolation); ok {
		t.Fatalf("events = %+v, want no schema violation for fenced JSON", stream.List())
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaValidationPassed); !ok {
		t.Fatalf("events = %+v, want schema validation passed", stream.List())
	}
}

func TestRunRepairsInvalidResultSchemaWithinBound(t *testing.T) {
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
				Usage:      provider.Usage{InputTokens: 2, OutputTokens: 1, CostUSD: 0.01},
			},
			{
				Text:       `{"status":"ok"}`,
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 3, OutputTokens: 2, CostUSD: 0.02},
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
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	if out.Text != `{"status":"ok"}` {
		t.Fatalf("Text = %q, want repaired JSON", out.Text)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 3 || out.Usage.CostUSD != 0.03 {
		t.Fatalf("Usage = %+v, want initial plus repair usage", out.Usage)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial and repair", len(p.requests))
	}
	repairReq := p.requests[1]
	if len(repairReq.Tools) != 0 {
		t.Fatalf("repair tools = %+v, want no tools", repairReq.Tools)
	}
	if !strings.Contains(repairReq.Messages[0].Text(), "Return only valid JSON") ||
		!strings.Contains(repairReq.Messages[0].Text(), contract.Name) {
		t.Fatalf("repair prompt = %q, want JSON-only schema repair prompt", repairReq.Messages[0].Text())
	}

	violations, err := store.SchemaViolations(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want original violation only", len(violations))
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaRepairStarted); !ok {
		t.Fatalf("events = %+v, want schema repair started event", stream.List())
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaRepairCompleted); !ok {
		t.Fatalf("events = %+v, want schema repair completed event", stream.List())
	}
	if event, ok := findEvent(stream.List(), events.EventSchemaValidationPassed); !ok || event.Payload["repaired"] != true {
		t.Fatalf("events = %+v, want repaired schema validation passed event", stream.List())
	}
	llmStarted := 0
	llmCompleted := 0
	repairLLMCompleted := false
	for _, event := range stream.List() {
		switch event.Kind {
		case events.EventLLMCallStarted:
			llmStarted++
		case events.EventLLMCallCompleted:
			llmCompleted++
			if event.Payload["model"] != "anthropic/claude-sonnet-4-6" {
				t.Fatalf("LLM completed payload = %+v, want requested model", event.Payload)
			}
			if event.Payload["repair"] == true && event.Payload["attempt"] == 1 {
				repairLLMCompleted = true
			}
		}
	}
	if llmStarted != 2 || llmCompleted != 2 || !repairLLMCompleted {
		t.Fatalf("events = %+v, want normal and repair LLM start/completion", stream.List())
	}
}

func TestRunFailsWhenSchemaRepairAttemptsAreExhausted(t *testing.T) {
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
			{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
			{Text: `{"status":2}`, StopReason: provider.StopEndTurn},
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
	if err == nil {
		t.Fatal("Run returned nil error after exhausted repair")
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial and one repair", len(p.requests))
	}
	violations, err := store.SchemaViolations(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("violations = %d, want original and repair violations", len(violations))
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaRepairFailed); !ok {
		t.Fatalf("events = %+v, want schema repair failed event", stream.List())
	}
	if _, ok := findEvent(stream.List(), events.EventSchemaValidationPassed); ok {
		t.Fatalf("events = %+v, did not want schema validation passed", stream.List())
	}
}

func TestRunPassesSchemaRepairEventsThroughHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventSchemaRepairStarted, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	contract, err := schema.FromType(reflect.TypeFor[harnessSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{
			{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
			{Text: `{"status":"ok"}`, StopReason: provider.StopEndTurn},
		},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithHookBus(hooks),
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
	if out.Status != harness.StatusCompleted {
		t.Fatalf("Status = %q, want completed", out.Status)
	}
	event, ok := findEvent(stream.List(), events.EventSchemaRepairStarted)
	if !ok {
		t.Fatalf("events = %+v, want schema repair started event", stream.List())
	}
	if event.Payload["checked"] != true {
		t.Fatalf("event payload = %+v, want hook enrichment", event.Payload)
	}
}

func TestRunEmitsSchemaValidationPassedWhenResultSchemaMatches(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	contract, err := schema.FromType(reflect.TypeFor[harnessSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       `{"status":"ok"}`,
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithResultSchema(contract),
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

	event, ok := findEvent(stream.List(), events.EventSchemaValidationPassed)
	if !ok {
		t.Fatalf("events = %+v, want schema validation passed event", stream.List())
	}
	if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["schema"] != contract.Name {
		t.Fatalf("event payload = %+v, want schema %q", event.Payload, contract.Name)
	}
	if _, ok := event.Payload["output"]; ok {
		t.Fatalf("event payload = %+v, must not include raw output", event.Payload)
	}
}

func TestRunPassesSchemaValidationPassedThroughHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventSchemaValidationPassed, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	contract, err := schema.FromType(reflect.TypeFor[harnessSchemaReply]())
	if err != nil {
		t.Fatalf("FromType returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       `{"status":"ok"}`,
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithHookBus(hooks),
		harness.WithResultSchema(contract),
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

	event, ok := findEvent(stream.List(), events.EventSchemaValidationPassed)
	if !ok {
		t.Fatalf("events = %+v, want schema validation passed event", stream.List())
	}
	if event.Payload["checked"] != true {
		t.Fatalf("event payload = %+v, want hook enrichment", event.Payload)
	}
}

func TestWithSchemaRepairAttemptsRejectsNegativeValue(t *testing.T) {
	p := &scriptedProvider{}
	_, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithSchemaRepairAttempts(-1),
	)
	if err == nil {
		t.Fatal("New returned nil error for negative repair attempts")
	}
}

func TestRunAppendsCoreEventsAndRunsHooks(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventBeforeLLM, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["hooked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
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

	recorded := stream.List()
	kinds := make([]events.EventKind, 0, len(recorded))
	for _, event := range recorded {
		kinds = append(kinds, event.Kind)
	}
	wantKinds := []events.EventKind{
		events.EventSessionStart,
		events.EventPipeStarted,
		events.EventBeforeLLM,
		events.EventAfterLLM,
		events.EventPipeCompleted,
		events.EventSessionEnd,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("event kinds = %+v, want %+v", kinds, wantKinds)
	}
	if recorded[2].Payload["hooked"] != true {
		t.Fatalf("before LLM payload = %+v, want hook enrichment", recorded[2].Payload)
	}
	if recorded[1].Payload["model"] != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("pipe started payload = %+v, want model", recorded[1].Payload)
	}
	if recorded[4].Payload["status"] != string(harness.StatusCompleted) {
		t.Fatalf("pipe completed payload = %+v, want completed status", recorded[4].Payload)
	}
}

func TestRunPersistsHookModifiedEventToStreamAndStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	observed := false
	if err := hooks.Register(events.EventBeforeLLM, func(ctx context.Context, event events.Event) (events.Event, error) {
		observed = true
		if event.ExecID == "" || event.SessionID == "" || event.TraceID == "" {
			t.Fatalf("hook event = %+v, want session identifiers before persistence", event)
		}
		event.Payload["hooked"] = "before-persistence"
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
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
	if !observed {
		t.Fatal("before-LLM hook was not called")
	}

	streamEvent, ok := findEvent(stream.List(), events.EventBeforeLLM)
	if !ok {
		t.Fatalf("stream events = %+v, want before-LLM event", stream.List())
	}
	persisted, err := store.Events(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	storeEvent, ok := findEvent(persisted, events.EventBeforeLLM)
	if !ok {
		t.Fatalf("persisted events = %+v, want before-LLM event", persisted)
	}
	if streamEvent.Payload["hooked"] != "before-persistence" {
		t.Fatalf("stream event payload = %+v, want hook enrichment", streamEvent.Payload)
	}
	if storeEvent.Payload["hooked"] != "before-persistence" {
		t.Fatalf("persisted event payload = %+v, want hook enrichment", storeEvent.Payload)
	}
	if streamEvent.ID == 0 || storeEvent.ID != streamEvent.ID {
		t.Fatalf("stream event ID = %d, persisted event ID = %d, want state store to receive appended event", streamEvent.ID, storeEvent.ID)
	}
}

func TestRunEmitsProviderResponseMetadataAfterLLM(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Metadata: provider.ResponseMetadata{
				Provider: "scripted",
				Model:    "anthropic/claude-sonnet-4-6",
				Latency:  12 * time.Millisecond,
			},
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
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

	event, ok := findEvent(stream.List(), events.EventAfterLLM)
	if !ok {
		t.Fatal("EventAfterLLM not emitted")
	}
	if event.Payload["provider"] != "scripted" ||
		event.Payload["model"] != "anthropic/claude-sonnet-4-6" ||
		event.Payload["provider_model"] != "anthropic/claude-sonnet-4-6" ||
		event.Payload["latency_ms"] != int64(12) {
		t.Fatalf("after LLM payload = %+v, want provider metadata", event.Payload)
	}
}

func TestRunEmitsPromptCacheMetadataAfterLLM(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Metadata: provider.ResponseMetadata{
				Provider: "scripted",
				Model:    "anthropic/claude-sonnet-4-6",
				PromptCache: provider.PromptCacheMetadata{
					Requested:        true,
					Supported:        true,
					Applied:          true,
					CacheKey:         "prompt:test-cache-key",
					ReadInputTokens:  17,
					WriteInputTokens: 23,
				},
			},
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
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

	event, ok := findEvent(stream.List(), events.EventAfterLLM)
	if !ok {
		t.Fatal("EventAfterLLM not emitted")
	}
	if event.Payload["prompt_cache_requested"] != true ||
		event.Payload["prompt_cache_supported"] != true ||
		event.Payload["prompt_cache_applied"] != true ||
		event.Payload["prompt_cache_read_tokens"] != 17 ||
		event.Payload["prompt_cache_write_tokens"] != 23 {
		t.Fatalf("after LLM payload = %+v, want prompt cache metadata", event.Payload)
	}
	if _, ok := event.Payload["prompt_cache_key"]; ok {
		t.Fatalf("after LLM payload = %+v, must not expose prompt cache key", event.Payload)
	}
}

func TestRunBlocksProviderCallWhenBeforeLLMHookFails(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	blocked := errors.New("policy denied LLM call")
	if err := hooks.Register(events.EventBeforeLLM, func(ctx context.Context, event events.Event) (events.Event, error) {
		return event, blocked
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "should not run",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithHookBus(hooks),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, blocked) {
		t.Fatalf("Run error = %v, want blocking hook error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if len(p.requests) != 0 {
		t.Fatalf("provider calls = %d, want none after blocking before-LLM hook", len(p.requests))
	}
	if _, ok := findEvent(stream.List(), events.EventBeforeLLM); ok {
		t.Fatalf("events = %+v, blocked before-LLM event must not be appended", stream.List())
	}
	event, ok := findEvent(stream.List(), events.EventPipeFailed)
	errorText, _ := event.Payload["error"].(string)
	if !ok ||
		event.Payload["status"] != string(harness.StatusFailed) ||
		!strings.Contains(errorText, "hook") {
		t.Fatalf("events = %+v, want pipe failed event after blocking hook", stream.List())
	}
}

func TestRunReturnsHookErrorBeforePersistingBlockedEvent(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	blocked := errors.New("audit hook denied token=secret123")
	if err := hooks.Register(events.EventAfterLLM, func(ctx context.Context, event events.Event) (events.Event, error) {
		return event, blocked
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	hookFailedHookCalled := false
	if err := hooks.Register(events.EventHookFailed, func(ctx context.Context, event events.Event) (events.Event, error) {
		hookFailedHookCalled = true
		return event, errors.New("hook_failed hook must not run")
	}); err != nil {
		t.Fatalf("Register hook_failed returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
		harness.WithHookBus(hooks),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, blocked) {
		t.Fatalf("Run error = %v, want blocking hook error", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if len(p.requests) != 1 {
		t.Fatalf("provider calls = %d, want one call before after-LLM hook blocks", len(p.requests))
	}
	if _, ok := findEvent(stream.List(), events.EventAfterLLM); ok {
		t.Fatalf("stream events = %+v, blocked after-LLM event must not be appended", stream.List())
	}
	if hookFailedHookCalled {
		t.Fatal("hook_failed hook was called; hook failure recording must bypass HookBus")
	}
	hookFailed, ok := findEvent(stream.List(), events.EventHookFailed)
	if !ok {
		t.Fatalf("stream events = %+v, want hook_failed event", stream.List())
	}
	if hookFailed.ExecID != out.Session.ExecID || hookFailed.SessionID != out.Session.SessionID || hookFailed.TraceID != out.Session.TraceID {
		t.Fatalf("hook_failed event = %+v, session = %+v, want matching identifiers", hookFailed, out.Session)
	}
	if hookFailed.Payload["blocked_kind"] != string(events.EventAfterLLM) {
		t.Fatalf("hook_failed payload = %+v, want blocked after-LLM kind", hookFailed.Payload)
	}
	hookErrorText, _ := hookFailed.Payload["error"].(string)
	if !strings.Contains(hookErrorText, "[REDACTED]") || strings.Contains(hookErrorText, "secret123") {
		t.Fatalf("hook_failed error = %q, want redacted secret", hookErrorText)
	}
	persisted, err := store.Events(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if _, ok := findEvent(persisted, events.EventAfterLLM); ok {
		t.Fatalf("persisted events = %+v, blocked after-LLM event must not be stored", persisted)
	}
	if persistedHookFailed, ok := findEvent(persisted, events.EventHookFailed); !ok || persistedHookFailed.Payload["blocked_kind"] != string(events.EventAfterLLM) {
		t.Fatalf("persisted events = %+v, want hook_failed event with blocked kind", persisted)
	}
	failed, ok := findEvent(stream.List(), events.EventPipeFailed)
	errorText, _ := failed.Payload["error"].(string)
	if !ok || !strings.Contains(errorText, "audit hook denied") {
		t.Fatalf("stream events = %+v, want pipe failed event with hook error", stream.List())
	}
}

func TestRunEmitsPipeFailedOnProviderError(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventPipeFailed, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["observed"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	boom := errors.New("provider exploded")
	p := &scriptedProvider{err: boom}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithHookBus(hooks),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want provider error", err)
	}
	event, ok := findEvent(stream.List(), events.EventPipeFailed)
	if !ok {
		t.Fatalf("events = %+v, want pipe failed event", stream.List())
	}
	if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["status"] != string(harness.StatusFailed) || event.Payload["observed"] != true {
		t.Fatalf("event payload = %+v, want failed status and hook enrichment", event.Payload)
	}
}

func TestRunEmitsSessionCancelledOnProviderCancellation(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	p := &scriptedProvider{err: context.Canceled}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	event, ok := findEvent(stream.List(), events.EventSessionCancelled)
	if !ok {
		t.Fatalf("events = %+v, want session cancelled event", stream.List())
	}
	if event.ExecID != out.Session.ExecID || event.SessionID != out.Session.SessionID || event.TraceID != out.Session.TraceID {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["status"] != string(harness.StatusFailed) || event.Payload["error"] == "" {
		t.Fatalf("event payload = %+v, want failed status and error", event.Payload)
	}
	persisted, err := store.Events(context.Background(), out.Session.ExecID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if _, ok := findEvent(persisted, events.EventSessionCancelled); !ok {
		t.Fatalf("persisted events = %+v, want session cancelled event", persisted)
	}
}

func findEvent(recorded []events.Event, kind events.EventKind) (events.Event, bool) {
	for _, event := range recorded {
		if event.Kind == kind {
			return event, true
		}
	}
	return events.Event{}, false
}

type harnessSchemaReply struct {
	Status string `json:"status"`
}

type blockingHarnessProvider struct {
	started chan struct{}
}

func (p *blockingHarnessProvider) Name() string {
	return "blocking"
}

func (p *blockingHarnessProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}
	<-ctx.Done()
	return provider.Response{}, ctx.Err()
}

func TestRunComputesCostFromPricingTable(t *testing.T) {
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 1000, OutputTokens: 500},
			Metadata: provider.ResponseMetadata{
				PromptCache: provider.PromptCacheMetadata{ReadInputTokens: 200, WriteInputTokens: 100},
			},
		}},
	}
	pricing := provider.PricingTable{
		"anthropic/claude-sonnet-4-6": provider.PerMillion(3, 15, 0.30, 3.75),
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithPricing(pricing),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	want := 1000*(3.0/1_000_000) + 500*(15.0/1_000_000) + 200*(0.30/1_000_000) + 100*(3.75/1_000_000)
	if diff := out.Usage.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("Usage.CostUSD = %v, want %v", out.Usage.CostUSD, want)
	}
}

func TestRunPricingTableMissingRateLeavesBestEffortCost(t *testing.T) {
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.02},
		}},
	}
	pricing := provider.PricingTable{
		"openai/gpt-4o": provider.PerMillion(2.5, 10, 0, 0),
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithPricing(pricing),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Usage.CostUSD != 0.02 {
		t.Fatalf("Usage.CostUSD = %v, want best-effort 0.02 preserved", out.Usage.CostUSD)
	}
}

func TestRunNoPricingTableLeavesCostUnchanged(t *testing.T) {
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.05},
		}},
	}
	h, err := harness.New(p, harness.WithModel("anthropic/claude-sonnet-4-6"))
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Usage.CostUSD != 0.05 {
		t.Fatalf("Usage.CostUSD = %v, want unchanged 0.05", out.Usage.CostUSD)
	}
}

func TestRunEmitsComputedCostOnLLMCallCompleted(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 1000, OutputTokens: 500},
		}},
	}
	pricing := provider.PricingTable{
		"anthropic/claude-sonnet-4-6": provider.PerMillion(3, 15, 0, 0),
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithPricing(pricing),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if _, err := h.Run(context.Background(), "payload"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	event, ok := findEvent(stream.List(), events.EventLLMCallCompleted)
	if !ok {
		t.Fatalf("llm_call_completed event not emitted")
	}
	cost, ok := event.Payload["cost_usd"].(float64)
	if !ok {
		t.Fatalf("cost_usd payload missing or wrong type: %#v", event.Payload["cost_usd"])
	}
	want := 1000*(3.0/1_000_000) + 500*(15.0/1_000_000)
	if diff := cost - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost_usd payload = %v, want %v", cost, want)
	}
}
