package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewAzureOpenAIRequiresAPIKeyAndBaseURL(t *testing.T) {
	if _, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{BaseURL: "https://r.openai.azure.com"}); err == nil {
		t.Fatal("NewAzureOpenAI returned nil error without API key")
	}
	if _, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{APIKey: "k"}); err == nil {
		t.Fatal("NewAzureOpenAI returned nil error without base URL")
	}

	p, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{APIKey: "k", BaseURL: "https://r.openai.azure.com"})
	if err != nil {
		t.Fatalf("NewAzureOpenAI returned error: %v", err)
	}
	if p.Name() != "azure" {
		t.Fatalf("Name = %q, want azure", p.Name())
	}
}

func TestAzureOpenAIShapesDeploymentURLAndAPIKeyHeader(t *testing.T) {
	var gotPath string
	var gotQuery string
	var gotAuth string
	var gotAPIKey string
	var gotModel string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("api-key")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"chatcmpl_test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":2}
		}`)
	}))
	defer server.Close()

	p, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{
		APIKey:     "secret-key",
		BaseURL:    server.URL,
		APIVersion: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("NewAzureOpenAI returned error: %v", err)
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "azure/gpt-4o-deploy",
		Messages: []provider.Message{provider.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text != "done" {
		t.Fatalf("Text = %q, want done", resp.Text)
	}
	if gotPath != "/openai/deployments/gpt-4o-deploy/chat/completions" {
		t.Fatalf("path = %q, want deployment-based path", gotPath)
	}
	if gotQuery != "api-version=2024-06-01" {
		t.Fatalf("query = %q, want api-version", gotQuery)
	}
	if gotAPIKey != "secret-key" {
		t.Fatalf("api-key header = %q, want secret-key", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization header = %q, want empty (Azure uses api-key)", gotAuth)
	}
	if gotModel != "gpt-4o-deploy" {
		t.Fatalf("model = %q, want deployment name", gotModel)
	}
}

func TestAzureOpenAIDefaultsAPIVersion(t *testing.T) {
	p, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{APIKey: "k", BaseURL: "https://r.openai.azure.com"})
	if err != nil {
		t.Fatalf("NewAzureOpenAI returned error: %v", err)
	}
	if p.APIVersion() != provider.DefaultAzureOpenAIAPIVersion {
		t.Fatalf("APIVersion = %q, want default %q", p.APIVersion(), provider.DefaultAzureOpenAIAPIVersion)
	}
}
