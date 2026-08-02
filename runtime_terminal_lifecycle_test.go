package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPPipelineTerminalFailureFailsExecutionAndKeepsDurableJournal(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(webhook.Close)

	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("test/model")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{
		provider: &httpScriptedProvider{response: provider.Response{
			Text:       `{"status":"classified"}`,
			StopReason: provider.StopEndTurn,
		}},
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		eventStream:  stream,
		durableRuns:  newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`)))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}

	assertOnlyFailedExecution(t, store)
	assertTerminalFailureEvents(t, stream.List())
	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 1 {
		t.Fatalf("journals after terminal failure = %+v, want retained run journal", journals)
	}
	intents, err := store.ToolIntents(context.Background(), journals[0].ExecID)
	if err != nil {
		t.Fatalf("ToolIntents returned error: %v", err)
	}
	if len(intents) != 1 || intents[0].ToolName != "ouvrier_push_webhook" || intents[0].StepIndex != 1 {
		t.Fatalf("terminal intents = %+v, want webhook intent at terminal step index", intents)
	}
}

func TestCronPipelineTerminalFailureFailsExecutionAndKeepsDurableJournal(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(webhook.Close)

	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Cron("@every 1h")),
		Pipe("summarize events", Model("test/model")),
		Push(Webhook(webhook.URL)),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	rt := httpRuntime{
		provider: &httpScriptedProvider{response: provider.Response{
			Text:       `{"status":"summarized"}`,
			StopReason: provider.StopEndTurn,
		}},
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		eventStream:  stream,
		durableRuns:  newDurableRunsConfig(0),
	}

	if _, err := runCronPlanOnce(context.Background(), rt, plans[0], time.Now().UTC()); err == nil {
		t.Fatal("runCronPlanOnce returned nil, want terminal failure")
	}
	assertOnlyFailedExecution(t, store)
	assertTerminalFailureEvents(t, stream.List())
	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 1 {
		t.Fatalf("journals after terminal failure = %+v, want retained run journal", journals)
	}
}

func TestHTTPPipelineCompletesAfterSuccessfulTerminalTool(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(webhook.Close)

	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("test/model")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{
		provider: &httpScriptedProvider{response: provider.Response{
			Text:       `{"status":"classified"}`,
			StopReason: provider.StopEndTurn,
		}},
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		eventStream:  stream,
		durableRuns:  newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`)))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	executions, err := store.Executions(context.Background())
	if err != nil {
		t.Fatalf("Executions returned error: %v", err)
	}
	if len(executions) != 1 || executions[0].Status != state.ExecutionCompleted {
		t.Fatalf("executions = %+v, want one completed execution", executions)
	}

	recorded := stream.List()
	terminalIndex := eventKindIndex(recorded, events.EventToolCallCompleted)
	completedIndex := eventKindIndex(recorded, events.EventPipelineCompleted)
	if terminalIndex < 0 || completedIndex < 0 || terminalIndex >= completedIndex {
		t.Fatalf("events = %+v, want terminal tool completion before pipeline completion", recorded)
	}
	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 0 {
		t.Fatalf("journals after successful terminal = %+v, want pruned", journals)
	}
}

func TestStreamPipelineTerminalFailureFailsExecutionAndKeepsDurableJournal(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(webhook.Close)

	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("nats://127.0.0.1:4222/tickets")),
		Pipe("summarize event", Model("test/model")),
		Push(Webhook(webhook.URL)),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	rt := httpRuntime{
		provider: &httpScriptedProvider{response: provider.Response{
			Text:       `{"status":"summarized"}`,
			StopReason: provider.StopEndTurn,
		}},
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		eventStream:  stream,
		durableRuns:  newDurableRunsConfig(0),
	}

	if _, err := runStreamPlanOnce(context.Background(), rt, plans[0], streamMessage{ID: "message-1", Body: `{"event":"created"}`}); err == nil {
		t.Fatal("runStreamPlanOnce returned nil, want terminal failure")
	}
	assertOnlyFailedExecution(t, store)
	assertTerminalFailureEvents(t, stream.List())
	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 1 {
		t.Fatalf("journals after terminal failure = %+v, want retained run journal", journals)
	}
}

func TestHTTPTriggerIdempotencyRetriesFailedTerminalThenDedupesSuccess(t *testing.T) {
	failTerminal := true
	terminalCalls := 0
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		terminalCalls++
		if failTerminal {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(webhook.Close)

	store := state.NewMemoryStore()
	nodes := []Node{
		From("POST /tickets", IdempotencyKey("X-Delivery-ID")),
		Pipe("classify ticket", Model("test/model")),
		Push(Webhook(webhook.URL)),
	}
	handler, err := newHTTPHandlerWithRuntime(nodes, httpRuntime{
		provider: &httpScriptedProvider{response: provider.Response{
			Text:       `{"status":"classified"}`,
			StopReason: provider.StopEndTurn,
		}},
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	request := func() *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
		req.Header.Set("X-Delivery-ID", "delivery-1")
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	if first := request(); first.Code != http.StatusBadGateway {
		t.Fatalf("first status = %d, want %d; body=%s", first.Code, http.StatusBadGateway, first.Body.String())
	}
	failTerminal = false
	if second := request(); second.Code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want %d; body=%s", second.Code, http.StatusAccepted, second.Body.String())
	}
	if third := request(); third.Code != http.StatusAccepted || !strings.Contains(third.Body.String(), "duplicate_idempotency_key") {
		t.Fatalf("duplicate response = %d %s, want accepted duplicate", third.Code, third.Body.String())
	}
	if terminalCalls != 2 {
		t.Fatalf("terminal calls = %d, want failed attempt plus successful retry only", terminalCalls)
	}

	plans, err := compilePlans(nodes)
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	key := triggerIdempotencyReservationKey(plans[0], "X-Delivery-ID", "delivery-1")
	record, ok, err := store.Idempotency(context.Background(), key)
	if err != nil || !ok || record.Outcome != state.IdempotencySucceeded {
		t.Fatalf("idempotency record = %+v ok=%v err=%v, want succeeded", record, ok, err)
	}
}

func assertOnlyFailedExecution(t *testing.T, store state.Store) {
	t.Helper()
	executions, err := store.Executions(context.Background())
	if err != nil {
		t.Fatalf("Executions returned error: %v", err)
	}
	if len(executions) != 1 || executions[0].Status != state.ExecutionFailed {
		t.Fatalf("executions = %+v, want one failed execution", executions)
	}
}

func assertTerminalFailureEvents(t *testing.T, recorded []events.Event) {
	t.Helper()
	if _, ok := findRuntimeHTTPEvent(recorded, events.EventPipelineFailed); !ok {
		t.Fatalf("events = %+v, want pipeline_failed", recorded)
	}
	if _, ok := findRuntimeHTTPEvent(recorded, events.EventPipelineCompleted); ok {
		t.Fatalf("events = %+v, must not contain pipeline_completed", recorded)
	}
}

func eventKindIndex(recorded []events.Event, kind events.EventKind) int {
	for index, event := range recorded {
		if event.Kind == kind {
			return index
		}
	}
	return -1
}
