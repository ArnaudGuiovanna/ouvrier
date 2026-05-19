package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/provider"
)

func TestHTTPAdminTriggerRequiresBearerTokenWhenConfigured(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for unauthorized admin trigger", len(scripted.requests))
	}
}

func TestHTTPAdminTriggerRunsExistingHTTPRouteThroughHarness(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Output != `{"status":"classified"}` {
		t.Fatalf("body = %+v, want ok classified output", body)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].Messages[0].Text() != `{"title":"broken"}` {
		t.Fatalf("provider input = %q, want trigger body JSON", scripted.requests[0].Messages[0].Text())
	}
}

func TestHTTPAdminTriggerMissingHTTPRouteReturnsNotFound(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/missing", `{"title":"broken"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "not_found" {
		t.Fatalf("status body = %q, want not_found", body.Status)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for missing route", len(scripted.requests))
	}
}

func TestHTTPAdminTriggerRejectsInvalidPayload(t *testing.T) {
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/trigger", strings.NewReader(`{"method":"POST"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "invalid_trigger" {
		t.Fatalf("status body = %q, want invalid_trigger", body.Status)
	}
}

func TestHTTPAdminTriggerRespectsAcceptedReplyAsync(t *testing.T) {
	scripted := &asyncAdminTriggerProvider{started: make(chan struct{})}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /jobs"),
		Pipe("process job", Model("anthropic/claude-sonnet-4-6")),
		Reply(Accepted()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "", "POST", "/jobs", `{"id":"J-1"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("async admin trigger did not start provider")
	}
}

func newTestAdminTriggerHTTPHandler(t *testing.T, rt httpRuntime) http.Handler {
	t.Helper()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	return handler
}

func newAdminTriggerRequest(t *testing.T, token, method, path, body string) *http.Request {
	t.Helper()
	payload, err := json.Marshal(struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}{
		Method: method,
		Path:   path,
		Body:   body,
	})
	if err != nil {
		t.Fatalf("Marshal admin trigger payload returned error: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/trigger", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

type asyncAdminTriggerProvider struct {
	started chan struct{}
}

func (p *asyncAdminTriggerProvider) Name() string {
	return "async-admin-trigger-provider"
}

func (p *asyncAdminTriggerProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	close(p.started)
	return provider.Response{Text: `{"status":"accepted"}`, StopReason: provider.StopEndTurn}, nil
}
