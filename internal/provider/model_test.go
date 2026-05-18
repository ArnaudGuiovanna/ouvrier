package provider_test

import (
	"testing"

	"ouvrier/internal/provider"
)

func TestParseModelIDRequiresProviderPrefix(t *testing.T) {
	ref, err := provider.ParseModelID("anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ParseModelID returned error: %v", err)
	}
	if ref.Provider != "anthropic" {
		t.Fatalf("Provider = %q, want anthropic", ref.Provider)
	}
	if ref.Name != "claude-sonnet-4-6" {
		t.Fatalf("Name = %q, want claude-sonnet-4-6", ref.Name)
	}

	for _, raw := range []string{"", "claude-sonnet-4-6", "anthropic/", "/claude-sonnet-4-6", "anthropic/claude/extra"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := provider.ParseModelID(raw); err == nil {
				t.Fatalf("ParseModelID(%q) returned nil error", raw)
			}
		})
	}
}
