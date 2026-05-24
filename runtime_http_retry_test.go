package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type transientOnceHTTPProvider struct {
	calls int
}

func (p *transientOnceHTTPProvider) Name() string {
	return "scripted"
}

func (p *transientOnceHTTPProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.calls++
	if p.calls == 1 {
		return provider.Response{}, provider.TransientError(errors.New("temporary provider failure"))
	}
	return provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn}, nil
}

func TestNewHTTPHandlerRetriesTransientProviderErrorWhenPipeRetryConfigured(t *testing.T) {
	scripted := &transientOnceHTTPProvider{}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Retry(1),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Output != `{"status":"classified"}` {
		t.Fatalf("output = %q, want classified JSON", body.Output)
	}
	if scripted.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", scripted.calls)
	}
}

func TestNewHTTPHandlerDoesNotRetryTransientProviderErrorWhenPipeRetryDisabled(t *testing.T) {
	scripted := &transientOnceHTTPProvider{}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Retry(0),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if scripted.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", scripted.calls)
	}
}
