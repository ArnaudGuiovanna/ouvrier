package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
)

type subAgentBudgetProvider struct {
	mu         sync.Mutex
	rootCalls  int
	childCalls int
}

func (p *subAgentBudgetProvider) Name() string {
	return "subagent-budget-scripted"
}

func (p *subAgentBudgetProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	switch req.Model {
	case "anthropic/claude-sonnet-4-6":
		p.mu.Lock()
		p.rootCalls++
		rootCalls := p.rootCalls
		p.mu.Unlock()
		if rootCalls > 1 {
			tokens := provider.Usage{InputTokens: 1, OutputTokens: 1}
			return provider.Response{Text: `{"status":"should-not-run"}`, StopReason: provider.StopEndTurn, Usage: tokens}, nil
		}
		return provider.Response{
			Text:       "need child work",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID:        "call_child",
				Name:      "expensive_child",
				Arguments: []byte(`{"input":"payload"}`),
			}},
			Usage: provider.Usage{InputTokens: 50, OutputTokens: 50, CostUSD: 0.01},
		}, nil
	case "anthropic/claude-haiku-4-5":
		p.mu.Lock()
		p.childCalls++
		p.mu.Unlock()
		return provider.Response{
			Text:       `{"text":"expensive"}`,
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 500001, OutputTokens: 0, CostUSD: 0.01},
		}, nil
	default:
		return provider.Response{}, nil
	}
}

func (p *subAgentBudgetProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rootCalls, p.childCalls
}

func TestSubAgentUsageConsumesParentBudget(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &subAgentBudgetProvider{}
	child := Pipeline(
		Pipe("expensive child",
			Model("anthropic/claude-haiku-4-5"),
			Output[httpSubAgentReply](),
		),
	)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /budget"),
		Pipe("parent",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("expensive_child", child, MaxParallel(1)),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/budget", strings.NewReader(`{"id":"B-1"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	rootCalls, childCalls := scripted.counts()
	if rootCalls != 1 || childCalls != 1 {
		t.Fatalf("provider calls root=%d child=%d, want one root and one child only", rootCalls, childCalls)
	}

	var budgetEvents []events.Event
	for _, event := range stream.List() {
		if event.Kind == events.EventBudgetExceeded {
			budgetEvents = append(budgetEvents, event)
		}
	}
	if len(budgetEvents) == 0 {
		t.Fatalf("events = %+v, want budget exceeded event", stream.List())
	}
	for _, event := range budgetEvents {
		if event.Payload["budget"] != "tokens" ||
			event.Payload["max_tokens"] != 500000 ||
			event.Payload["used_tokens"] != 500101 {
			t.Fatalf("budget event = %+v, want aggregate parent+child tokens", event)
		}
	}
	if len(budgetEvents) < 2 {
		t.Fatalf("budget events = %+v, want child and parent budget enforcement", budgetEvents)
	}
	if budgetEvents[0].ExecID != budgetEvents[1].ExecID || budgetEvents[0].TraceID != budgetEvents[1].TraceID {
		t.Fatalf("budget events = %+v, want shared exec and trace", budgetEvents)
	}
}
