package provider_test

import (
	"testing"

	"ouvrier/internal/provider"
)

func TestNewVLLMDefaultsBaseURLAndDoesNotRequireAPIKey(t *testing.T) {
	p, err := provider.NewVLLM(provider.VLLMConfig{})
	if err != nil {
		t.Fatalf("NewVLLM returned error: %v", err)
	}
	if p.Name() != "vllm" {
		t.Fatalf("Name = %q, want vllm", p.Name())
	}
	if p.BaseURL() != provider.DefaultVLLMBaseURL {
		t.Fatalf("BaseURL = %q, want %q", p.BaseURL(), provider.DefaultVLLMBaseURL)
	}
}

func TestVLLMCompletePostsChatCompletionAndParsesTextResponseWithoutAuth(t *testing.T) {
	runCompatChatCompletionTest(t, "vllm", "qwen2.5-coder", "", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewVLLM(provider.VLLMConfig{BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewVLLM returned error: %v", err)
		}
		return p
	})
}

func TestVLLMCompleteSendsOptionalAPIKey(t *testing.T) {
	runCompatChatCompletionTest(t, "vllm", "qwen2.5-coder", "Bearer local-key", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewVLLM(provider.VLLMConfig{APIKey: "local-key", BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewVLLM returned error: %v", err)
		}
		return p
	})
}

func TestVLLMCompleteParsesToolCallResponse(t *testing.T) {
	runCompatToolCallResponseTest(t, "vllm", "qwen2.5-coder", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewVLLM(provider.VLLMConfig{BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewVLLM returned error: %v", err)
		}
		return p
	})
}

func TestVLLMCompleteRejectsForeignModel(t *testing.T) {
	runCompatForeignModelTest(t, "vllm", func(t *testing.T, baseURL string) compatProvider {
		t.Helper()
		p, err := provider.NewVLLM(provider.VLLMConfig{BaseURL: baseURL})
		if err != nil {
			t.Fatalf("NewVLLM returned error: %v", err)
		}
		return p
	})
}
