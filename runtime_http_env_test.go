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
		"GROQ_API_KEY",
		"GROQ_BASE_URL",
		"DEEPSEEK_API_KEY",
		"DEEPSEEK_BASE_URL",
		"AZURE_OPENAI_API_KEY",
		"AZURE_OPENAI_BASE_URL",
		"AZURE_OPENAI_API_VERSION",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_REGION",
		"AWS_BEDROCK_BASE_URL",
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
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"status\":\"env provider\"}"}],"stop_reason":"end_turn"}`))
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
	if body.Status != "ok" || body.Output != `{"status":"env provider"}` {
		t.Fatalf("body = %+v, want ok env provider JSON", body)
	}
}

func TestProviderRegistryFromEnvRoutesAllV01ProviderPrefixes(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("MISTRAL_API_KEY", "mistral-key")
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("GROQ_API_KEY", "groq-key")
	t.Setenv("DEEPSEEK_API_KEY", "deepseek-key")
	t.Setenv("AZURE_OPENAI_API_KEY", "azure-key")
	t.Setenv("AZURE_OPENAI_BASE_URL", "https://example.openai.azure.com")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("AWS_REGION", "us-east-1")

	registry, err := providerRegistryFromEnv()
	if err != nil {
		t.Fatalf("providerRegistryFromEnv returned error: %v", err)
	}
	tests := []struct {
		model string
		want  string
	}{
		{model: "anthropic/claude-sonnet-4-6", want: "anthropic"},
		{model: "openai/gpt-4.1-mini", want: "openai"},
		{model: "ollama/llama3.1", want: "ollama"},
		{model: "mistral/mistral-large-latest", want: "mistral"},
		{model: "gemini/gemini-2.0-flash", want: "gemini"},
		{model: "vllm/qwen2.5-coder", want: "vllm"},
		{model: "groq/llama-3.3-70b-versatile", want: "groq"},
		{model: "deepseek/deepseek-chat", want: "deepseek"},
		{model: "azure/gpt-4o-deploy", want: "azure"},
		{model: "bedrock/anthropic.claude-3-5-sonnet-20240620-v1:0", want: "bedrock"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			provider, err := registry.ForModel(tt.model)
			if err != nil {
				t.Fatalf("ForModel(%q) returned error: %v", tt.model, err)
			}
			if provider.Name() != tt.want {
				t.Fatalf("provider = %q, want %q", provider.Name(), tt.want)
			}
		})
	}
}
