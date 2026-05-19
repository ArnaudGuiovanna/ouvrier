package harness_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/harness"
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/schema"
	"ouvrier/internal/state"
	"ouvrier/internal/tools"
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

func TestRunRetriesTransientProviderErrorBeforeSideEffects(t *testing.T) {
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
}

func TestRunDoesNotRetryTransientProviderErrorAfterToolCall(t *testing.T) {
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
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		return harnessLookupResult{Answer: "workers"}, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithProviderRetries(2),
		harness.WithToolExecutor(executor),
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
		events.EventBeforeLLM,
		events.EventAfterLLM,
		events.EventSessionEnd,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("event kinds = %+v, want %+v", kinds, wantKinds)
	}
	if recorded[1].Payload["hooked"] != true {
		t.Fatalf("before LLM payload = %+v, want hook enrichment", recorded[1].Payload)
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
