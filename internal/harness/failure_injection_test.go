package harness_test

// Failure-injection suite for the harness runLoop (slice 0A.2). Each test
// drives the loop into a provider- or caller-induced failure and asserts the
// harness fails deterministically: a stable Outcome status, terminal events on
// the trace, and execution state persisted even when the surrounding context
// is already cancelled (the WithoutCancel pattern from 0A.1).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// cancelAwareProvider is a scriptedProvider that honors context cancellation
// the way every real HTTP adapter does: a cancelled context fails the call
// with ctx.Err() before any scripted response is served.
type cancelAwareProvider struct {
	scriptedProvider
}

func (p *cancelAwareProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	return p.scriptedProvider.Complete(ctx, req)
}

func newFailureInjectionLookupExecutor(t *testing.T, executions *int, onExecute func()) *tools.Executor {
	t.Helper()
	executor := tools.NewExecutor()
	if err := executor.Register("lookup", func(ctx context.Context, args harnessLookupArgs) (harnessLookupResult, error) {
		*executions++
		if onExecute != nil {
			onExecute()
		}
		return harnessLookupResult{Answer: "workers"}, nil
	}, tools.WithMetadata(tools.Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	return executor
}

func assertTerminalFailureEvents(t *testing.T, recorded []events.Event) {
	t.Helper()
	pipeFailed, ok := findEvent(recorded, events.EventPipeFailed)
	if !ok {
		t.Fatalf("events = %+v, want pipe failed event", recorded)
	}
	if pipeFailed.Payload["status"] != string(harness.StatusFailed) {
		t.Fatalf("pipe failed payload = %+v, want failed status", pipeFailed.Payload)
	}
	if _, ok := findEvent(recorded, events.EventSessionEnd); !ok {
		t.Fatalf("events = %+v, want session end event", recorded)
	}
}

func assertPersistedExecution(t *testing.T, store *state.MemoryStore, execID string, want state.ExecutionStatus) {
	t.Helper()
	execution, ok, err := store.Execution(context.Background(), execID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want persisted execution")
	}
	if execution.Status != want || execution.CompletedAt.IsZero() {
		t.Fatalf("Execution = %+v, want %q with completion timestamp", execution, want)
	}
}

// A transport error on the second provider call — after a successful first
// iteration that executed tool calls — must fail the run deterministically:
// no retry beyond the configured budget, terminal events emitted, execution
// persisted as failed.
func TestRunFailsDeterministicallyOnTransportErrorMidLoop(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	transportErr := provider.TransientError(errors.New("anthropic request: connection reset by peer"))
	p := &scriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
				Usage:      provider.Usage{InputTokens: 3, OutputTokens: 2},
			},
			{},
		},
		errors: []error{nil, transportErr},
	}
	executions := 0
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(newFailureInjectionLookupExecutor(t, &executions, nil)),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
		harness.WithProviderRetries(0),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(context.Background(), "payload")
	if err == nil {
		t.Fatal("Run returned nil error, want transport failure")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("error = %v, want transport error surfaced", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if out.Iterations != 2 {
		t.Fatalf("Iterations = %d, want 2", out.Iterations)
	}
	if executions != 1 {
		t.Fatalf("tool executions = %d, want first-iteration lookup only", executions)
	}
	if len(p.requests) != 2 {
		t.Fatalf("provider calls = %d, want exactly 2 (no retries beyond budget)", len(p.requests))
	}

	failure, ok := findEvent(stream.List(), events.EventLLMCallFailed)
	if !ok {
		t.Fatalf("events = %+v, want llm call failed event", stream.List())
	}
	if failure.Payload["retrying"] != false || failure.Payload["transient"] != true {
		t.Fatalf("llm call failed payload = %+v, want final transient failure", failure.Payload)
	}
	assertTerminalFailureEvents(t, stream.List())
	assertPersistedExecution(t, store, out.Session.ExecID, state.ExecutionFailed)
}

// Cancelling the caller context while a tool is executing must still finish
// the run with a deterministic failed status, and the terminal events plus
// execution record must survive the cancellation (WithoutCancel pattern).
func TestRunCancelledDuringToolExecutionStillEmitsTerminalEventsAndPersists(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"ouvrier"}`),
	}
	p := &cancelAwareProvider{scriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need lookup",
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{call},
			},
			{
				Text:       "done",
				StopReason: provider.StopEndTurn,
			},
		},
	}}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executions := 0
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(newFailureInjectionLookupExecutor(t, &executions, cancel)),
		harness.WithStateStore(store),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	out, err := h.Run(runCtx, "payload")
	if err == nil {
		t.Fatal("Run returned nil error, want cancellation failure")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if out.Status != harness.StatusFailed {
		t.Fatalf("Status = %q, want failed", out.Status)
	}
	if executions != 1 {
		t.Fatalf("tool executions = %d, want the in-flight tool to finish once", executions)
	}

	cancelled, ok := findEvent(stream.List(), events.EventSessionCancelled)
	if !ok {
		t.Fatalf("events = %+v, want session cancelled event", stream.List())
	}
	if cancelled.Payload["status"] != string(harness.StatusFailed) {
		t.Fatalf("session cancelled payload = %+v, want failed status", cancelled.Payload)
	}
	assertTerminalFailureEvents(t, stream.List())
	assertPersistedExecution(t, store, out.Session.ExecID, state.ExecutionFailed)
}

// Malformed or empty provider response bodies must fail the run
// deterministically through a real HTTP adapter: a decode error, no partial
// tool execution, terminal events, and a persisted failed execution.
func TestRunFailsDeterministicallyOnMalformedProviderResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "malformed_json", body: `{"choices":[{"message":{"role":"assistant","content":"done"`},
		{name: "empty_body", body: ""},
		{name: "no_choices", body: `{}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()

			p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
			if err != nil {
				t.Fatalf("NewOpenAI returned error: %v", err)
			}
			store := state.NewMemoryStore()
			stream, err := events.NewEventStream()
			if err != nil {
				t.Fatalf("NewEventStream returned error: %v", err)
			}
			executions := 0
			h, err := harness.New(p,
				harness.WithModel("openai/gpt-4o-mini"),
				harness.WithToolExecutor(newFailureInjectionLookupExecutor(t, &executions, nil)),
				harness.WithStateStore(store),
				harness.WithEventStream(stream),
				harness.WithProviderRetries(0),
			)
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}

			out, err := h.Run(context.Background(), "payload")
			if err == nil {
				t.Fatal("Run returned nil error, want decode failure")
			}
			if out.Status != harness.StatusFailed {
				t.Fatalf("Status = %q, want failed", out.Status)
			}
			if executions != 0 {
				t.Fatalf("tool executions = %d, want no partial tool execution", executions)
			}
			if len(out.ToolCalls) != 0 {
				t.Fatalf("recorded tool calls = %d, want none", len(out.ToolCalls))
			}
			assertTerminalFailureEvents(t, stream.List())
			assertPersistedExecution(t, store, out.Session.ExecID, state.ExecutionFailed)
		})
	}
}

// A StopMaxTokens response on the very first iteration that carries partial
// tool calls must truncate the run: the tool calls stay observable on the
// trace (after_llm event) but are never started, and the truncated execution
// is persisted. Extends TestRunDoesNotExecuteToolCallsFromMaxTokensResponse
// with the observability and persistence half of the contract.
func TestRunFirstIterationMaxTokensKeepsToolCallsObservableWithoutExecution(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup",
		Arguments: []byte(`{"query":"partial"}`),
	}
	p := &scriptedProvider{
		responses: []provider.Response{{
			StopReason: provider.StopMaxTokens,
			ToolCalls:  []provider.ToolCall{call},
			Usage:      provider.Usage{InputTokens: 2, OutputTokens: 6},
		}},
	}
	executions := 0
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithToolExecutor(newFailureInjectionLookupExecutor(t, &executions, nil)),
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
	if out.Iterations != 1 {
		t.Fatalf("Iterations = %d, want 1", out.Iterations)
	}
	if executions != 0 {
		t.Fatalf("tool executions = %d, want none for a truncated first turn", executions)
	}
	if len(out.ToolCalls) != 0 {
		t.Fatalf("recorded tool calls = %d, want no accepted tool calls", len(out.ToolCalls))
	}

	afterLLM, ok := findEvent(stream.List(), events.EventAfterLLM)
	if !ok {
		t.Fatalf("events = %+v, want after llm event", stream.List())
	}
	if afterLLM.Payload["tool_calls"] != 1 || afterLLM.Payload["stop_reason"] != string(provider.StopMaxTokens) {
		t.Fatalf("after llm payload = %+v, want observable truncated tool call", afterLLM.Payload)
	}
	if _, ok := findEvent(stream.List(), events.EventBeforeTool); ok {
		t.Fatalf("events = %+v, did not want any tool start event", stream.List())
	}
	budget, ok := findEvent(stream.List(), events.EventBudgetExceeded)
	if !ok {
		t.Fatalf("events = %+v, want budget exceeded event", stream.List())
	}
	if budget.Payload["budget"] != "provider_max_tokens" || budget.Payload["output_tokens"] != 6 {
		t.Fatalf("budget payload = %+v, want provider max-token details", budget.Payload)
	}
	assertPersistedExecution(t, store, out.Session.ExecID, state.ExecutionTruncated)
}
