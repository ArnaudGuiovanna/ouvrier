package provider_test

import (
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewMistralRequiresAPIKeyAndDefaultsBaseURL(t *testing.T) {
	_, err := provider.NewMistral(provider.MistralConfig{})
	if err == nil {
		t.Fatal("NewMistral returned nil error without API key")
	}

	p, err := provider.NewMistral(provider.MistralConfig{APIKey: " test-key "})
	if err != nil {
		t.Fatalf("NewMistral returned error: %v", err)
	}
	if p.Name() != "mistral" {
		t.Fatalf("Name = %q, want mistral", p.Name())
	}
	if p.BaseURL() != provider.DefaultMistralBaseURL {
		t.Fatalf("BaseURL = %q, want %q", p.BaseURL(), provider.DefaultMistralBaseURL)
	}
}

func TestMistralCompletePostsChatCompletionAndParsesTextResponse(t *testing.T) {
	runCompatChatCompletionTest(t, "mistral", "mistral-large-latest", "Bearer test-key", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewMistral(provider.MistralConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewMistral returned error: %v", err)
		}
		return p
	})
}

func TestMistralCompleteParsesToolCallResponse(t *testing.T) {
	runCompatToolCallResponseTest(t, "mistral", "mistral-large-latest", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewMistral(provider.MistralConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewMistral returned error: %v", err)
		}
		return p
	})
}

func TestMistralCompleteRejectsForeignModel(t *testing.T) {
	runCompatForeignModelTest(t, "mistral", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewMistral(provider.MistralConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewMistral returned error: %v", err)
		}
		return p
	})
}
