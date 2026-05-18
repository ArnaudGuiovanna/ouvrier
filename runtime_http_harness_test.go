package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/provider"
)

type httpScriptedProvider struct {
	requests []provider.Request
	response provider.Response
	err      error
}

func (p *httpScriptedProvider) Name() string {
	return "scripted"
}

func (p *httpScriptedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return provider.Response{}, p.err
	}
	return p.response, nil
}

func TestNewHTTPHandlerExecutesPipeThroughHarnessRuntime(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: "classified", StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" || body.Output != "classified" {
		t.Fatalf("body = %+v, want ok classified", body)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].Messages[0].Text() != `{"title":"broken"}` {
		t.Fatalf("provider input = %q", scripted.requests[0].Messages[0].Text())
	}
}
