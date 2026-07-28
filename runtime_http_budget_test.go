package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPIterationBudgetFailureIsIdentifiable(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{
				Text:       "first tool call",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID: "call_1", Name: "lookup", Arguments: []byte(`{}`),
				}},
			},
			{
				Text:       "second tool call",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID: "call_2", Name: "lookup", Arguments: []byte(`{}`),
				}},
			},
			{Text: "must not run", StopReason: provider.StopEndTurn},
		},
	}
	nodes := []Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup", func(context.Context) error { return nil }, ReadOnly()),
		),
		Reply(JSON[httpTestReply]()),
	}
	handler := newBudgetHTTPHandler(t, nodes, httpRuntime{
		provider: scripted, eventStream: stream, stateStore: store,
	}, 2)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`)))

	assertHTTPBudgetFailure(t, rec, "iterations")
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(scripted.requests))
	}
	assertPersistedHTTPBudget(t, store, "iterations")
}

func TestHTTPTokenBudgetFailureIsIdentifiable(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		response: provider.Response{
			Text:       `{"status":"partial"}`,
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), MaxTokens(1)),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

	assertHTTPBudgetFailure(t, rec, "tokens")
	assertPersistedHTTPBudget(t, store, "tokens")
}

func TestHTTPCostBudgetFailureIsIdentifiable(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		response: provider.Response{
			Text:       `{"status":"partial"}`,
			StopReason: provider.StopEndTurn,
			Usage:      provider.Usage{CostUSD: 0.02},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), MaxCostUSD(0.01)),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

	assertHTTPBudgetFailure(t, rec, "cost_usd")
	assertPersistedHTTPBudget(t, store, "cost_usd")
}

func TestHTTPWallClockBudgetFailureIsBoundedIdentifiableAndPersisted(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	store := state.NewMemoryStore()
	scripted := &pipeTimeoutProvider{started: make(chan struct{})}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), Timeout("20ms")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	startedAt := time.Now()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))
	elapsed := time.Since(startedAt)

	assertHTTPBudgetFailure(t, rec, "wallclock")
	if elapsed >= 5*time.Second {
		t.Fatalf("elapsed = %s, want bounded wallclock failure under 5s", elapsed)
	}
	select {
	case <-scripted.started:
	default:
		t.Fatal("provider was not called")
	}
	recorded := assertPersistedHTTPBudget(t, store, "wallclock")
	assertRuntimeHTTPEventOrder(t, recorded, []events.EventKind{
		events.EventBeforeLLM,
		events.EventBudgetExceeded,
	})
}

func TestHTTPClientCancellationIsNotReportedAsWallClockBudget(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &pipeTimeoutProvider{started: make(chan struct{})}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), Timeout("1s")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/tickets", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()
	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not stop after client cancellation")
	}

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "pipeline_execution_failed" || body.Budget != "" {
		t.Fatalf("body = %+v, want generic cancellation failure without budget", body)
	}
	if event, ok := findRuntimeHTTPEvent(stream.List(), events.EventBudgetExceeded); ok {
		t.Fatalf("unexpected budget event after client cancellation: %+v", event)
	}
}

func TestHTTPBudgetFailureIsIdentifiableOnAdminAndSSE(t *testing.T) {
	t.Run("admin trigger", func(t *testing.T) {
		t.Setenv("OUVRIER_ENV", "dev")
		scripted := &httpScriptedProvider{
			response: provider.Response{
				Text:       "partial",
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 2},
			},
		}
		handler, err := newHTTPHandlerWithRuntime([]Node{
			From("POST /tickets"),
			Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), MaxTokens(1)),
			Reply(JSON[httpTestReply]()),
		}, httpRuntime{provider: scripted})
		if err != nil {
			t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, newAdminTriggerRequest(t, "", http.MethodPost, "/tickets", `{"title":"broken"}`))

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
		}
		var body adminTriggerResponse
		decodeAdminJSON(t, rec, &body)
		if body.Status != "pipeline_execution_incomplete" || body.Budget != "tokens" {
			t.Fatalf("body = %+v, want incomplete tokens budget", body)
		}
	})

	t.Run("SSE", func(t *testing.T) {
		scripted := &httpScriptedProvider{
			response: provider.Response{
				Text:       "partial",
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 2},
			},
		}
		handler, err := newHTTPHandlerWithRuntime([]Node{
			From("POST /tickets"),
			Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), MaxTokens(1)),
			Reply(SSE()),
		}, httpRuntime{provider: scripted})
		if err != nil {
			t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want SSE %d", rec.Code, rec.Body.String(), http.StatusOK)
		}
		want := "event: error\ndata: {\"status\":\"pipeline_execution_incomplete\",\"budget\":\"tokens\"}\n\n"
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("SSE body missing %q:\n%s", want, rec.Body.String())
		}
	})
}

func TestHTTPApprovedResumePersistsIdentifiableBudgetFailure(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	call := provider.ToolCall{ID: "call_1", Name: "wire_payment"}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: "need approval", StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{call}},
			{
				Text:       "partial after resume",
				StopReason: provider.StopEndTurn,
				Usage:      provider.Usage{InputTokens: 2},
			},
		},
	}
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /payments"),
		Pipe("settle",
			Model("anthropic/claude-sonnet-4-6"),
			MaxTokens(1),
			Tool("wire_payment", func(context.Context) error { return nil }, RequiresApproval()),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	initial := httptest.NewRecorder()
	handler.ServeHTTP(initial, httptest.NewRequest(http.MethodPost, "/payments", nil))
	if initial.Code != http.StatusAccepted {
		t.Fatalf("initial status = %d body=%s, want %d", initial.Code, initial.Body.String(), http.StatusAccepted)
	}
	var suspended struct {
		ApprovalID string `json:"approval_id"`
		ExecID     string `json:"exec_id"`
	}
	decodeAdminJSON(t, initial, &suspended)
	if suspended.ApprovalID == "" || suspended.ExecID == "" {
		t.Fatalf("suspended response = %+v, want approval and execution ids", suspended)
	}

	approval := httptest.NewRecorder()
	handler.ServeHTTP(approval, httptest.NewRequest(
		http.MethodPost,
		"/admin/approvals/"+suspended.ApprovalID,
		strings.NewReader(`{"decision":"approve","decided_by":"qa"}`),
	))
	if approval.Code != http.StatusOK {
		t.Fatalf("approval status = %d body=%s, want %d", approval.Code, approval.Body.String(), http.StatusOK)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		recorded, err := store.Events(context.Background(), suspended.ExecID)
		if err != nil {
			t.Fatalf("Events returned error: %v", err)
		}
		for _, event := range recorded {
			if event.Kind == events.EventPipelineFailed && event.Payload["resumed"] == true {
				if event.Payload["budget"] != "tokens" {
					t.Fatalf("resumed failure = %+v, want tokens budget", event)
				}
				if errorText, _ := event.Payload["error"].(string); !strings.Contains(errorText, "budget=tokens") {
					t.Fatalf("resumed failure error = %q, want stable tokens diagnostic", errorText)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %+v, want resumed pipeline failure with tokens budget", recorded)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHTTPBudgetDiagnosticsAreProviderIndependentAtHarnessSeam(t *testing.T) {
	for _, model := range []string{
		"anthropic/claude-sonnet-4-6",
		"gemini/gemini-2.5-pro",
		"ollama/qwen3",
		"openai/gpt-5.2",
	} {
		t.Run(strings.SplitN(model, "/", 2)[0], func(t *testing.T) {
			scripted := &httpScriptedProvider{
				name: model,
				response: provider.Response{
					Text:       "partial",
					StopReason: provider.StopEndTurn,
					Usage:      provider.Usage{OutputTokens: 2},
				},
			}
			handler, err := newHTTPHandlerWithRuntime([]Node{
				From("POST /tickets"),
				Pipe("classify ticket", Model(model), MaxTokens(1)),
				Reply(JSON[httpTestReply]()),
			}, httpRuntime{provider: scripted})
			if err != nil {
				t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))
			assertHTTPBudgetFailure(t, rec, "tokens")
		})
	}
}

func newBudgetHTTPHandler(t *testing.T, nodes []Node, rt httpRuntime, maxIterations int) http.Handler {
	t.Helper()
	routes, err := httpRoutesFromNodes(nodes)
	if err != nil {
		t.Fatalf("httpRoutesFromNodes returned error: %v", err)
	}
	if len(routes) != 1 || len(routes[0].plan.Steps) != 1 {
		t.Fatalf("routes = %+v, want one route with one step", routes)
	}
	routes[0].plan.Steps[0].Budget.MaxIterations = maxIterations
	handler, err := newHTTPHandlerFromRoutes(routes, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerFromRoutes returned error: %v", err)
	}
	return handler
}

func assertHTTPBudgetFailure(t *testing.T, rec *httptest.ResponseRecorder, wantBudget string) {
	t.Helper()
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusBadGateway)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "pipeline_execution_incomplete" || body.Budget != wantBudget {
		t.Fatalf("body = %+v, want incomplete %s budget", body, wantBudget)
	}
}

func assertPersistedHTTPBudget(t *testing.T, store state.Store, wantBudget string) []events.Event {
	t.Helper()
	recorded, err := store.EventsSince(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("EventsSince returned error: %v", err)
	}
	event, ok := findRuntimeHTTPEvent(recorded, events.EventBudgetExceeded)
	if !ok || event.Payload["budget"] != wantBudget {
		t.Fatalf("events = %+v, want persisted %s budget event", recorded, wantBudget)
	}
	return recorded
}
