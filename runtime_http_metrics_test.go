package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func appendMetricsEvent(t *testing.T, stream *events.EventStream, event events.Event) {
	t.Helper()
	if _, err := stream.Append(context.Background(), event); err != nil {
		t.Fatalf("append event: %v", err)
	}
}

func metricsRuntimeWithEvents(t *testing.T) httpRuntime {
	t.Helper()
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	// A complete pipeline with a pipe, an llm call (with latency), a completed
	// tool call and a failed tool call.
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventPipelineStarted, ExecID: "e1", TraceID: "t1"})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventPipeStarted, ExecID: "e1", TraceID: "t1"})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventLLMCallStarted, ExecID: "e1", TraceID: "t1", Payload: map[string]any{"call_id": "c1"}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventLLMCallCompleted, ExecID: "e1", TraceID: "t1", Payload: map[string]any{"call_id": "c1", "latency_ms": 120}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventToolCallStarted, ExecID: "e1", TraceID: "t1", Payload: map[string]any{"tool_call_id": "tc1"}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventToolCallCompleted, ExecID: "e1", TraceID: "t1", Payload: map[string]any{"tool_call_id": "tc1"}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventToolCallStarted, ExecID: "e1", TraceID: "t1", Payload: map[string]any{"tool_call_id": "tc2"}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventToolCallFailed, ExecID: "e1", TraceID: "t1", Payload: map[string]any{"tool_call_id": "tc2", "error": "boom"}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventPipeCompleted, ExecID: "e1", TraceID: "t1"})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventPipelineCompleted, ExecID: "e1", TraceID: "t1"})
	return httpRuntime{stateStore: store, eventStream: stream}
}

func TestHTTPMetricsRequiresAdminAuth(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "")
	rt := metricsRuntimeWithEvents(t)
	rt.adminToken = "secret-token"
	handler := newTestAdminHTTPHandler(t, rt)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d for missing token", rec.Code, http.StatusUnauthorized)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d with valid token", rec.Code, http.StatusOK)
	}
}

func TestHTTPMetricsEmitsPrometheusFamilies(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	rt := metricsRuntimeWithEvents(t)
	handler := newTestAdminHTTPHandler(t, rt)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain exposition format", ct)
	}
	body := rec.Body.String()

	wantContains := []string{
		"# HELP ouvrier_pipeline_started_total",
		"# TYPE ouvrier_pipeline_started_total counter",
		"ouvrier_pipeline_started_total 1",
		"ouvrier_pipeline_completed_total 1",
		"# TYPE ouvrier_pipe_started_total counter",
		"ouvrier_pipe_completed_total 1",
		"ouvrier_llm_call_started_total 1",
		"ouvrier_llm_call_completed_total 1",
		"ouvrier_tool_call_started_total 2",
		"ouvrier_tool_call_completed_total 1",
		"ouvrier_tool_call_failed_total 1",
		"# TYPE ouvrier_llm_call_duration_ms summary",
		"ouvrier_llm_call_duration_ms_sum 120",
		"ouvrier_llm_call_duration_ms_count 1",
	}
	for _, want := range wantContains {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q\n---\n%s", want, body)
		}
	}
}

func TestHTTPMetricsDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventLLMCallStarted, ExecID: "e1", Payload: map[string]any{"call_id": "c1", "api_key": "sk-super-secret"}})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventLLMCallCompleted, ExecID: "e1", Payload: map[string]any{"call_id": "c1"}})
	rt := httpRuntime{stateStore: store, eventStream: stream}
	handler := newTestAdminHTTPHandler(t, rt)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "sk-super-secret") {
		t.Fatalf("metrics leaked secret:\n%s", rec.Body.String())
	}
}
