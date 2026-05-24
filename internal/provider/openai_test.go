package provider_test

import (
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewOpenAIRequiresAPIKeyAndDefaultsBaseURL(t *testing.T) {
	_, err := provider.NewOpenAI(provider.OpenAIConfig{})
	if err == nil {
		t.Fatal("NewOpenAI returned nil error without API key")
	}

	p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: " test-key "})
	if err != nil {
		t.Fatalf("NewOpenAI returned error: %v", err)
	}
	if p.Name() != "openai" {
		t.Fatalf("Name = %q, want openai", p.Name())
	}
	if p.BaseURL() != provider.DefaultOpenAIBaseURL {
		t.Fatalf("BaseURL = %q, want %q", p.BaseURL(), provider.DefaultOpenAIBaseURL)
	}
}

func TestOpenAICompletePostsChatCompletionAndParsesTextResponse(t *testing.T) {
	runCompatChatCompletionTest(t, "openai", "gpt-4.1-mini", "Bearer test-key", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewOpenAI returned error: %v", err)
		}
		return p
	})
}

func TestOpenAICompleteParsesToolCallResponse(t *testing.T) {
	runCompatToolCallResponseTest(t, "openai", "gpt-4.1-mini", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewOpenAI returned error: %v", err)
		}
		return p
	})
}

func TestOpenAICompleteRejectsForeignModel(t *testing.T) {
	runCompatForeignModelTest(t, "openai", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewOpenAI returned error: %v", err)
		}
		return p
	})
}
