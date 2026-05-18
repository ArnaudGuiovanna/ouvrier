package provider_test

import (
	"context"
	"errors"
	"testing"

	"ouvrier/internal/provider"
)

func TestNewAnthropicRequiresAPIKey(t *testing.T) {
	_, err := provider.NewAnthropic(provider.AnthropicConfig{})
	if err == nil {
		t.Fatal("NewAnthropic returned nil error without API key")
	}
}

func TestAnthropicProviderIsStructuredButDoesNotCallNetworkYet(t *testing.T) {
	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}
	if p.Name() != "anthropic" {
		t.Fatalf("Name = %q, want anthropic", p.Name())
	}
	if p.BaseURL() != "https://api.anthropic.com" {
		t.Fatalf("BaseURL = %q, want default Anthropic endpoint", p.BaseURL())
	}

	_, err = p.Complete(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if !errors.Is(err, provider.ErrNotImplemented) {
		t.Fatalf("Complete error = %v, want ErrNotImplemented", err)
	}
}
