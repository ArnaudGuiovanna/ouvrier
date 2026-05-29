package provider_test

import (
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewGroqRequiresAPIKeyAndDefaultsBaseURL(t *testing.T) {
	_, err := provider.NewGroq(provider.GroqConfig{})
	if err == nil {
		t.Fatal("NewGroq returned nil error without API key")
	}

	p, err := provider.NewGroq(provider.GroqConfig{APIKey: " test-key "})
	if err != nil {
		t.Fatalf("NewGroq returned error: %v", err)
	}
	if p.Name() != "groq" {
		t.Fatalf("Name = %q, want groq", p.Name())
	}
	if p.BaseURL() != provider.DefaultGroqBaseURL {
		t.Fatalf("BaseURL = %q, want %q", p.BaseURL(), provider.DefaultGroqBaseURL)
	}
}

func TestGroqCompletePostsChatCompletionAndParsesTextResponse(t *testing.T) {
	runCompatChatCompletionTest(t, "groq", "llama-3.3-70b-versatile", "Bearer test-key", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewGroq(provider.GroqConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewGroq returned error: %v", err)
		}
		return p
	})
}

func TestGroqCompleteParsesToolCallResponse(t *testing.T) {
	runCompatToolCallResponseTest(t, "groq", "llama-3.3-70b-versatile", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewGroq(provider.GroqConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewGroq returned error: %v", err)
		}
		return p
	})
}

func TestGroqCompleteRejectsForeignModel(t *testing.T) {
	runCompatForeignModelTest(t, "groq", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewGroq(provider.GroqConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewGroq returned error: %v", err)
		}
		return p
	})
}
