package cli

import "testing"

func TestResolveAgentModelPrefersExplicitProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	m, id, err := resolveAgentModel("anthropic/claude-sonnet-4-6", func() bool { return false })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m == nil || id != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("expected anthropic model, got id=%q model=%v", id, m)
	}
}

func TestResolveAgentModelPrefersCodexWhenSignedIn(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m, id, err := resolveAgentModel("", func() bool { return true })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m == nil || id != "codex/gpt-5-codex" {
		t.Fatalf("expected codex/gpt-5-codex when signed in, got id=%q model=%v", id, m)
	}
}

func TestResolveAgentModelNoneWhenNothingAvailable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m, _, _ := resolveAgentModel("", func() bool { return false })
	if m != nil {
		t.Fatalf("expected nil model when no auth/keys, got %v", m)
	}
}
