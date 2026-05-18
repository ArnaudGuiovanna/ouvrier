package ovr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clearProviderEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_BASE_URL",
		"OPENAI_API_KEY",
		"OPENAI_BASE_URL",
		"MISTRAL_API_KEY",
		"MISTRAL_BASE_URL",
		"GEMINI_API_KEY",
		"GEMINI_BASE_URL",
		"OLLAMA_BASE_URL",
		"VLLM_API_KEY",
		"VLLM_BASE_URL",
	} {
		t.Setenv(key, "")
	}
}

func TestNewHTTPHandlerUsesDefaultProviderRegistryFromEnv(t *testing.T) {
	clearProviderEnv(t)

	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", req.URL.Path)
		}
		gotKey = req.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"env provider"}],"stop_reason":"end_turn"}`))
	}))
	defer server.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)

	handler, err := newHTTPHandler([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if gotKey != "test-key" {
		t.Fatalf("x-api-key = %q, want test-key", gotKey)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" || body.Output != "env provider" {
		t.Fatalf("body = %+v, want ok env provider", body)
	}
}
