package ovr

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPMetricsCountersRemainMonotonicAcrossEventStreamEviction(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	stream, err := events.NewEventStream(events.WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	for i := 0; i < 3; i++ {
		appendMetricsEvent(t, stream, events.Event{
			Kind:   events.EventPipelineStarted,
			ExecID: "exec_" + strconv.Itoa(i),
		})
	}
	rt := httpRuntime{eventStream: stream}
	handler := newTestAdminHTTPHandler(t, rt)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first metrics status = %d, want 200", first.Code)
	}
	assertPrometheusSample(t, first.Body.String(), "ouvrier_pipeline_started_total", "3")

	appendMetricsEvent(t, stream, events.Event{
		Kind:   events.EventPipelineCompleted,
		ExecID: "exec_2",
	})
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second metrics status = %d, want 200", second.Code)
	}
	assertPrometheusSample(t, second.Body.String(), "ouvrier_pipeline_started_total", "3")
	assertPrometheusSample(t, second.Body.String(), "ouvrier_pipeline_completed_total", "1")
}

func TestHTTPMetricsSummariesRemainMonotonicAcrossEventStreamEviction(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	stream, err := events.NewEventStream(events.WithRetentionLimit(1))
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	appendMetricsEvent(t, stream, events.Event{
		Kind:    events.EventLLMCallCompleted,
		ExecID:  "exec_1",
		Payload: map[string]any{"latency_ms": 10},
	})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventSinkLogged, ExecID: "exec_1"})
	rt := httpRuntime{eventStream: stream}
	handler := newTestAdminHTTPHandler(t, rt)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assertPrometheusSample(t, first.Body.String(), "ouvrier_llm_call_duration_ms_sum", "10")
	assertPrometheusSample(t, first.Body.String(), "ouvrier_llm_call_duration_ms_count", "1")

	appendMetricsEvent(t, stream, events.Event{
		Kind:    events.EventLLMCallCompleted,
		ExecID:  "exec_2",
		Payload: map[string]any{"latency_ms": 20},
	})
	appendMetricsEvent(t, stream, events.Event{Kind: events.EventSinkLogged, ExecID: "exec_2"})
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assertPrometheusSample(t, second.Body.String(), "ouvrier_llm_call_duration_ms_sum", "30")
	assertPrometheusSample(t, second.Body.String(), "ouvrier_llm_call_duration_ms_count", "2")
}

func TestAdminEventsUsesDurableHistoryBeyondEventStreamRetention(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	stream, err := events.NewEventStream(events.WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	store := state.NewMemoryStore()
	rt := httpRuntime{eventStream: stream, stateStore: store}
	for i, kind := range []events.EventKind{
		events.EventPipelineStarted,
		events.EventPipeStarted,
		events.EventPipelineCompleted,
	} {
		if err := rt.appendRuntimeEvent(context.Background(), events.Event{
			Kind:   kind,
			ExecID: "exec_retained",
			Payload: map[string]any{
				"sequence": i + 1,
			},
		}); err != nil {
			t.Fatalf("appendRuntimeEvent %d: %v", i, err)
		}
	}
	if got := len(stream.List()); got != 2 {
		t.Fatalf("in-memory retained events = %d, want 2", got)
	}

	handler := newTestAdminHTTPHandler(t, rt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/events?follow=false", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin events status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAdminEventStreamIDs(t, rec.Body.String(), 1, 2, 3)

	trace := httptest.NewRecorder()
	handler.ServeHTTP(trace, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_retained", nil))
	if trace.Code != http.StatusOK {
		t.Fatalf("admin trace status = %d, want 200; body=%s", trace.Code, trace.Body.String())
	}
	var body struct {
		Events []struct {
			ID uint64 `json:"id"`
		} `json:"events"`
	}
	decodeAdminJSON(t, trace, &body)
	if len(body.Events) != 3 {
		t.Fatalf("admin trace events = %+v, want full durable history of 3", body.Events)
	}
}

func TestAdminEventsFallbackReturnsRetainedWindowInOrder(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	stream, err := events.NewEventStream(events.WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := stream.Append(context.Background(), events.Event{Kind: events.EventBeforeTool}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/events?follow=false", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin events status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertAdminEventStreamIDs(t, rec.Body.String(), 2, 3)
}

func TestAdminEventsStorelessSSEReportsCursorGapExplicitly(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	stream, err := events.NewEventStream(events.WithRetentionLimit(2))
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := stream.Append(context.Background(), events.Event{Kind: events.EventBeforeTool}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/events?format=sse&follow=false&after_id=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin events status = %d, want streaming 200; body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "event: error\n") || !strings.Contains(body, `"status":"event_history_gap"`) {
		t.Fatalf("admin SSE body = %q, want explicit event_history_gap", body)
	}
}

func TestAdminEventBatchDoesNotAdvanceCursorOnWriteFailure(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	if _, err := stream.Append(context.Background(), events.Event{Kind: events.EventBeforeTool}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	cursor := uint64(0)
	err = (httpRuntime{eventStream: stream}).writeAdminEventStreamBatch(context.Background(), failingSSEWriter{}, "sse", &cursor)
	if err == nil {
		t.Fatal("writeAdminEventStreamBatch returned nil error, want writer failure")
	}
	if cursor != 0 {
		t.Fatalf("cursor = %d, want 0 so the event is not silently lost", cursor)
	}
}

func assertPrometheusSample(t *testing.T, body, name, value string) {
	t.Helper()
	want := name + " " + value
	for _, line := range strings.Split(body, "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("metrics body missing sample %q:\n%s", want, body)
}

func assertAdminEventStreamIDs(t *testing.T, body string, want ...uint64) {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	got := make([]uint64, 0, len(want))
	for scanner.Scan() {
		var event struct {
			ID uint64 `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode admin event %q: %v", scanner.Text(), err)
		}
		got = append(got, event.ID)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan admin events: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("admin event IDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("admin event IDs = %v, want %v", got, want)
		}
	}
}
