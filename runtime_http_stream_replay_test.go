package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

func streamReplayPlans(t *testing.T) []runtimeplan.Plan {
	t.Helper()
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), StreamDLQ("kafka://tickets.dlq", 1)),
		Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	return plans
}

func TestHTTPAdminStreamReplayRequiresAuth(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "")
	dlq := newMemoryStreamDLQ()
	handler, err := newAdminHandlerWithRuntime(streamReplayPlans(t), httpRuntime{
		streamDLQ:  dlq,
		adminToken: "secret",
	})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/streams/replay", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/streams/replay", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad-token status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestHTTPAdminStreamReplayDrainsDLQ(t *testing.T) {
	stream, _ := events.NewEventStream()
	plans := streamReplayPlans(t)
	prov := &httpScriptedProvider{response: provider.Response{Text: `{"status":"replayed"}`, StopReason: provider.StopEndTurn}}
	dlq := newMemoryStreamDLQ()
	if err := dlq.Route(context.Background(), "kafka://tickets.dlq", streamMessage{ID: "poison-1", Body: `{"event":"created"}`}); err != nil {
		t.Fatalf("seed Route returned error: %v", err)
	}

	t.Setenv("OUVRIER_ENV", "dev")
	handler, err := newAdminHandlerWithRuntime(plans, httpRuntime{
		provider:    prov,
		eventStream: stream,
		streamDLQ:   dlq,
	})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/streams/replay", strings.NewReader(`{"uri":"kafka://tickets"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}
	var body struct {
		Status   string `json:"status"`
		Replayed int    `json:"replayed"`
		DLQ      string `json:"dlq"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Replayed != 1 {
		t.Fatalf("body = %+v, want ok with 1 replayed", body)
	}
	if dlq.Depth("kafka://tickets.dlq") != 0 {
		t.Fatalf("DLQ depth after replay = %d, want 0", dlq.Depth("kafka://tickets.dlq"))
	}
}

func TestHTTPAdminStreamReplayUnknownStreamReturnsNotFound(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	handler, err := newAdminHandlerWithRuntime(streamReplayPlans(t), httpRuntime{streamDLQ: newMemoryStreamDLQ()})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/streams/replay", strings.NewReader(`{"uri":"kafka://nonexistent"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
