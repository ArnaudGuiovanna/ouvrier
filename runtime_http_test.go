package ovr

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type httpTestReply struct {
	Status string `json:"status"`
}

func TestNewHTTPHandlerServesDirectGETReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status body = %q, want ok", body.Status)
	}
}

func TestNewHTTPHandlerServesDirectPOSTReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("POST /tickets"),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewHTTPHandlerServesDirectAcceptedReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("POST /jobs"),
		Reply(Accepted()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jobs", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "accepted" {
		t.Fatalf("status body = %q, want accepted", body.Status)
	}
}

func TestNewHTTPHandlerServesHTTPPathParams(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /tickets/{id}"),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tickets/123", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewHTTPHandlerServesDirectSinkAsAccepted(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("POST /events"),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/events", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestNewHTTPHandlerServesMultipleHTTPPipelines(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
		From("POST /events"),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
	}

	events := httptest.NewRecorder()
	handler.ServeHTTP(events, httptest.NewRequest(http.MethodPost, "/events", nil))
	if events.Code != http.StatusAccepted {
		t.Fatalf("events status = %d, want %d", events.Code, http.StatusAccepted)
	}
}

func TestNewHTTPHandlerReturnsProviderStatusWhenPipeProviderMissing(t *testing.T) {
	clearProviderEnv(t)

	handler, err := newHTTPHandler([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "provider_not_configured" {
		t.Fatalf("status body = %q, want provider_not_configured", body.Status)
	}
}

func TestNewHTTPHandlerRejectsNonHTTPPipeline(t *testing.T) {
	_, err := newHTTPHandler([]Node{
		From(Cron("0 6 * * *")),
		Sink(Log()),
	})
	if !errors.Is(err, ErrRunNotImplemented) {
		t.Fatalf("newHTTPHandler error = %v, want ErrRunNotImplemented", err)
	}
}
