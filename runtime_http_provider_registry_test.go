package ovr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewHTTPHandlerRoutesStepsByModelProvider(t *testing.T) {
	anthropic := &httpScriptedProvider{
		name:     "anthropic",
		response: provider.Response{Text: "anthropic output", StopReason: provider.StopEndTurn},
	}
	openai := &httpScriptedProvider{
		name:     "openai",
		response: provider.Response{Text: `{"status":"openai output"}`, StopReason: provider.StopEndTurn},
	}
	registry, err := provider.NewRegistry(anthropic, openai)
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Pipe("rewrite response", Model("openai/gpt-4.1-mini")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{providers: registry})
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
	if body.Status != "ok" || body.Output != `{"status":"openai output"}` {
		t.Fatalf("body = %+v, want ok openai output JSON", body)
	}
	if len(anthropic.requests) != 1 {
		t.Fatalf("anthropic calls = %d, want 1", len(anthropic.requests))
	}
	if len(openai.requests) != 1 {
		t.Fatalf("openai calls = %d, want 1", len(openai.requests))
	}
	if anthropic.requests[0].Messages[0].Text() != `{"title":"broken"}` {
		t.Fatalf("anthropic input = %q", anthropic.requests[0].Messages[0].Text())
	}
	if openai.requests[0].Messages[0].Text() != "anthropic output" {
		t.Fatalf("openai input = %q, want anthropic output", openai.requests[0].Messages[0].Text())
	}
}
