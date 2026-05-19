package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/events"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/state"
)

func TestHTTPAdminHealthAllowsDevAccessWithoutToken(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store, eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status      string `json:"status"`
		StateStore  bool   `json:"state_store"`
		EventStream bool   `json:"event_stream"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || !body.StateStore || !body.EventStream {
		t.Fatalf("body = %+v, want ok with store and stream", body)
	}
}

func TestHTTPAdminRequiresBearerTokenWhenConfigured(t *testing.T) {
	handler := newTestAdminHTTPHandler(t, httpRuntime{adminToken: "secret-admin-token"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/health", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if strings.Contains(unauthorized.Body.String(), "secret-admin-token") {
		t.Fatalf("unauthorized response echoed admin token: %q", unauthorized.Body.String())
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.Code, http.StatusOK)
	}
}

func TestHTTPAdminStatusSummarizesStateStoreExecutions(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      state.ExecutionCompleted,
		StartedAt:   now,
		CompletedAt: now.Add(time.Second),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
	})
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_2",
		TraceID:   "trace_2",
		Status:    state.ExecutionRunning,
		StartedAt: now.Add(time.Minute),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_2",
		SessionID: "sess_2",
		TraceID:   "trace_2",
		Model:     "openai/gpt-4.1",
		StartedAt: now.Add(time.Minute),
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status     string         `json:"status"`
		Sessions   int            `json:"sessions"`
		Executions int            `json:"executions"`
		ByStatus   map[string]int `json:"by_status"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Sessions != 2 || body.Executions != 2 {
		t.Fatalf("body = %+v, want two sessions and executions", body)
	}
	if body.ByStatus["completed"] != 1 || body.ByStatus["running"] != 1 {
		t.Fatalf("by_status = %+v, want completed/running counts", body.ByStatus)
	}
}

func TestHTTPAdminTracesListsRecentTraceSummariesFromEventStream(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallCompleted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces?last=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			ExecID      string `json:"exec_id"`
			TraceID     string `json:"trace_id"`
			Events      int    `json:"events"`
			LastEventID uint64 `json:"last_event_id"`
			LastKind    string `json:"last_kind"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one trace", body)
	}
	if body.Traces[0].ExecID != "exec_2" || body.Traces[0].TraceID != "trace_2" || body.Traces[0].Events != 2 {
		t.Fatalf("trace = %+v, want exec_2 summary", body.Traces[0])
	}
	if body.Traces[0].LastEventID != 3 || body.Traces[0].LastKind != string(events.EventLLMCallCompleted) {
		t.Fatalf("trace last event = %+v, want event 3 llm completion", body.Traces[0])
	}
}

func TestHTTPAdminTraceByExecutionIncludesExecutionAndEvents(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      state.ExecutionFailed,
		StartedAt:   now,
		CompletedAt: now.Add(time.Second),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
	})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventPipeFailed, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_other", SessionID: "sess_other", TraceID: "trace_other"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store, eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status    string `json:"status"`
		Execution struct {
			ExecID  string `json:"exec_id"`
			TraceID string `json:"trace_id"`
			Status  string `json:"status"`
		} `json:"execution"`
		Events []struct {
			ExecID string `json:"exec_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
		Sessions int `json:"sessions"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Execution.ExecID != "exec_1" || body.Execution.Status != "failed" {
		t.Fatalf("body = %+v, want failed exec_1", body)
	}
	if len(body.Events) != 2 || body.Events[0].ExecID != "exec_1" || body.Events[1].Kind != string(events.EventPipeFailed) {
		t.Fatalf("events = %+v, want only exec_1 events", body.Events)
	}
	if body.Sessions != 1 {
		t.Fatalf("sessions = %d, want 1", body.Sessions)
	}
}

func newTestAdminHTTPHandler(t *testing.T, rt httpRuntime) http.Handler {
	t.Helper()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	return handler
}

func decodeAdminJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
}

func saveAdminExecution(t *testing.T, store state.Store, execution state.Execution) {
	t.Helper()
	if err := store.SaveExecution(context.Background(), execution); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
}

func saveAdminSession(t *testing.T, store state.Store, session runtimecore.Session) {
	t.Helper()
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("SaveSession returned error: %v", err)
	}
}

func appendAdminEvent(t *testing.T, stream *events.EventStream, event events.Event) {
	t.Helper()
	if _, err := stream.Append(context.Background(), event); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
}
