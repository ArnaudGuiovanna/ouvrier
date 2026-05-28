package provider_test

import (
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewDeepSeekRequiresAPIKeyAndDefaultsBaseURL(t *testing.T) {
	_, err := provider.NewDeepSeek(provider.DeepSeekConfig{})
	if err == nil {
		t.Fatal("NewDeepSeek returned nil error without API key")
	}

	p, err := provider.NewDeepSeek(provider.DeepSeekConfig{APIKey: " test-key "})
	if err != nil {
		t.Fatalf("NewDeepSeek returned error: %v", err)
	}
	if p.Name() != "deepseek" {
		t.Fatalf("Name = %q, want deepseek", p.Name())
	}
	if p.BaseURL() != provider.DefaultDeepSeekBaseURL {
		t.Fatalf("BaseURL = %q, want %q", p.BaseURL(), provider.DefaultDeepSeekBaseURL)
	}
}

func TestDeepSeekCompletePostsChatCompletionAndParsesTextResponse(t *testing.T) {
	runCompatChatCompletionTest(t, "deepseek", "deepseek-chat", "Bearer test-key", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewDeepSeek(provider.DeepSeekConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewDeepSeek returned error: %v", err)
		}
		return p
	})
}

func TestDeepSeekCompleteParsesToolCallResponse(t *testing.T) {
	runCompatToolCallResponseTest(t, "deepseek", "deepseek-chat", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewDeepSeek(provider.DeepSeekConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewDeepSeek returned error: %v", err)
		}
		return p
	})
}

func TestDeepSeekCompleteRejectsForeignModel(t *testing.T) {
	runCompatForeignModelTest(t, "deepseek", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewDeepSeek(provider.DeepSeekConfig{APIKey: "test-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewDeepSeek returned error: %v", err)
		}
		return p
	})
}
