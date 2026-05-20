package harness_test

import (
	"context"
	"sync"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/harness"
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
)

func TestSharedBudgetLedgerCountsUsageAcrossHarnesses(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	ledger := harness.NewBudgetLedger(runtimecore.Budget{
		MaxIterations: 5,
		MaxTokens:     5,
		MaxCostUSD:    1,
	})

	firstProvider := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "first",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 2, OutputTokens: 1, CostUSD: 0.10},
		}},
	}
	first, err := harness.New(firstProvider,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithBudgetLedger(ledger),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	firstOut, err := first.Run(context.Background(), "first")
	if err != nil {
		t.Fatalf("first Run returned error: %v", err)
	}
	if firstOut.Status != harness.StatusCompleted {
		t.Fatalf("first status = %q, want completed", firstOut.Status)
	}

	secondProvider := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "second",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 3, OutputTokens: 0, CostUSD: 0.10},
		}},
	}
	second, err := harness.New(secondProvider,
		harness.WithModel("anthropic/claude-haiku-4-5"),
		harness.WithBudgetLedger(ledger),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	secondOut, err := second.Run(context.Background(), "second")
	if err != nil {
		t.Fatalf("second Run returned error: %v", err)
	}
	if secondOut.Status != harness.StatusTruncated {
		t.Fatalf("second status = %q, want truncated", secondOut.Status)
	}

	event, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded", stream.List())
	}
	if event.Payload["budget"] != "tokens" ||
		event.Payload["max_tokens"] != 5 ||
		event.Payload["used_tokens"] != 6 ||
		event.SessionID != secondOut.Session.SessionID {
		t.Fatalf("budget event = %+v, want aggregate token usage on second session", event)
	}
}

func TestRunDoesNotCallProviderWhenSharedBudgetAlreadyExceeded(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	ledger := harness.NewBudgetLedger(runtimecore.Budget{MaxIterations: 5, MaxTokens: 2, MaxCostUSD: 1})
	ledger.Add(provider.Usage{InputTokens: 2, OutputTokens: 1})
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "should not run",
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1},
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithBudgetLedger(ledger),
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
	if out.Iterations != 0 {
		t.Fatalf("Iterations = %d, want no LLM iteration", out.Iterations)
	}
	if len(p.requests) != 0 {
		t.Fatalf("provider calls = %d, want none after exhausted budget", len(p.requests))
	}

	event, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded", stream.List())
	}
	if event.Payload["budget"] != "tokens" ||
		event.Payload["max_tokens"] != 2 ||
		event.Payload["used_tokens"] != 3 ||
		event.SessionID != out.Session.SessionID {
		t.Fatalf("budget event = %+v, want exhausted shared budget on current session", event)
	}
}

func TestBudgetLedgerRecordsConcurrentUsageExactly(t *testing.T) {
	ledger := harness.NewBudgetLedger(runtimecore.Budget{MaxTokens: 99})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ledger.Add(provider.Usage{InputTokens: 1, OutputTokens: 1, CostUSD: 0.01})
		}()
	}
	wg.Wait()

	usage, payload, exceeded := ledger.Exceeded()
	if usage.InputTokens != 50 || usage.OutputTokens != 50 {
		t.Fatalf("usage = %+v, want exact concurrent aggregate", usage)
	}
	if !exceeded {
		t.Fatal("exceeded = false, want token budget exceeded")
	}
	if payload["budget"] != "tokens" ||
		payload["max_tokens"] != 99 ||
		payload["used_tokens"] != 100 {
		t.Fatalf("payload = %+v, want deterministic token payload", payload)
	}
}
