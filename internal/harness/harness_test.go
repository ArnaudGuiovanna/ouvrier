package harness_test

import (
	"context"
	"errors"
	"testing"

	"ouvrier/internal/harness"
	"ouvrier/internal/provider"
	"ouvrier/internal/state"
)

type scriptedProvider struct {
	responses []provider.Response
	err       error
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
	if len(p.responses) == 0 {
		return provider.Response{}, nil
	}
	idx := len(p.requests) - 1
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
