package ovr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPRuntimeSubAgentChildUsageExceedsLowParentTokenBudget(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	parent, err := runtimeplan.NewSession("anthropic/claude-sonnet-4-6",
		runtimeplan.WithSessionIDs("exec_low_parent_budget", "sess_low_parent_budget", "trace_low_parent_budget"),
		runtimeplan.WithSessionBudget(runtimeplan.Budget{
			MaxIterations: 5,
			MaxTokens:     10,
			MaxCostUSD:    1,
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

	scripted := &lowParentBudgetSubAgentProvider{}
	child := Pipeline(
		Pipe("child under its own budget",
			Model("anthropic/claude-haiku-4-5"),
			Output[httpSubAgentReply](),
		),
	)
	plans, err := compilePlans([]Node{
		From("POST /budget-low"),
		Pipe("parent under budget before child",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("budget_child", child, MaxParallel(1)),
		),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	handler := newLowParentBudgetHTTPHandler(plans[0], httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: stream,
	}, parent, harness.NewBudgetLedger(parent.Budget))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/budget-low", strings.NewReader(`{"id":"B-2"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pipeline_execution_incomplete") {
		t.Fatalf("body = %s, want pipeline_execution_incomplete", rec.Body.String())
	}
	rootCalls, childCalls := scripted.counts()
	if rootCalls != 1 || childCalls != 1 {
		t.Fatalf("provider calls root=%d child=%d, want first root call and child call only", rootCalls, childCalls)
	}

	event, ok := findBudgetExceededForExecTrace(stream.List(), parent.ExecID, parent.TraceID)
	if !ok {
		t.Fatalf("events = %+v, want budget_exceeded on exec %q trace %q", stream.List(), parent.ExecID, parent.TraceID)
	}
	if event.Payload["budget"] != "tokens" ||
		event.Payload["max_tokens"] != 10 ||
		event.Payload["used_tokens"] != 11 {
		t.Fatalf("budget event = %+v, want aggregate parent+child token usage max=10 used=11", event)
	}
}

func newLowParentBudgetHTTPHandler(plan runtimeplan.Plan, rt httpRuntime, parent runtimeplan.Session, ledger *harness.BudgetLedger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		input, err := readHTTPRequestInput(req)
		if err != nil {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
			return
		}

		output, err := rt.runSteps(req.Context(), plan.Steps, input, planRunScope{
			parentSession: &parent,
			budgetLedger:  ledger,
		})
		if err != nil {
			switch {
			case errors.Is(err, errHTTPProviderNotConfigured):
				writeJSONStatus(w, http.StatusServiceUnavailable, "provider_not_configured")
			case errors.Is(err, errHTTPPipelineIncomplete):
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_incomplete")
			default:
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			}
			return
		}
		if err := validateTerminalReplyOutput(plan, output); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONOutput(w, http.StatusOK, "ok", output)
	})
}

type lowParentBudgetSubAgentProvider struct {
	mu         sync.Mutex
	rootCalls  int
	childCalls int
}

func (p *lowParentBudgetSubAgentProvider) Name() string {
	return "low-parent-budget-subagent-scripted"
}

func (p *lowParentBudgetSubAgentProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
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
			return provider.Response{
				Text:       `{"status":"should-not-run"}`,
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1},
			}, nil
		}
		return provider.Response{
			Text:       "need child work",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID:        "call_low_budget_child",
				Name:      "budget_child",
				Arguments: []byte(`{"input":"payload"}`),
			}},
			Usage: provider.Usage{InputTokens: 4, OutputTokens: 1},
		}, nil
	case "anthropic/claude-haiku-4-5":
		p.mu.Lock()
		p.childCalls++
		p.mu.Unlock()
		return provider.Response{
			Text:       `{"text":"child done"}`,
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 5, OutputTokens: 1},
		}, nil
	default:
		return provider.Response{}, nil
	}
}

func (p *lowParentBudgetSubAgentProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rootCalls, p.childCalls
}

func findBudgetExceededForExecTrace(eventsList []events.Event, execID, traceID string) (events.Event, bool) {
	for _, event := range eventsList {
		if event.Kind == events.EventBudgetExceeded && event.ExecID == execID && event.TraceID == traceID {
			return event, true
		}
	}
	return events.Event{}, false
}
