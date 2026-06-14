package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	codexprovider "github.com/ArnaudGuiovanna/ouvrier/internal/provider/codex"
)

// operateModelFromEnv builds the Ouvrier-owned tool-calling model transport for
// `--model provider/id`, resolving the provider from the matching API-key env
// var. It returns (nil, nil) when no model is requested so the cockpit falls
// back to the deterministic keyword planner.
func operateModelFromEnv(modelID string) (operate.AgentModel, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}

	var providers []provider.Provider
	if key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); key != "" {
		p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: key, BaseURL: os.Getenv("ANTHROPIC_BASE_URL")})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: key, BaseURL: os.Getenv("OPENAI_BASE_URL")})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}

	registry, err := provider.NewRegistry(providers...)
	if err != nil {
		return nil, err
	}
	p, err := registry.ForModel(modelID)
	if err != nil {
		return nil, fmt.Errorf("%w: operate --model %q needs the matching API key (ANTHROPIC_API_KEY or OPENAI_API_KEY): %w", ErrUsage, modelID, err)
	}
	return operate.NewProviderModel(p, modelID), nil
}

// resolveAgentModel chooses the agent model transport, auth-first:
//  1. an explicit --model provider/x when that provider's API key is present;
//  2. a signed-in Codex subscription (zero key) via the codex exec transport;
//  3. an API-key provider from env (anthropic/openai);
//  4. nil (the cockpit shows the sign-in card; the planner remains the fallback).
//
// signedIn is injected so tests don't shell out to codex.
func resolveAgentModel(modelID string, signedIn func() bool) (operate.AgentModel, string, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID != "" && !strings.HasPrefix(modelID, "codex/") {
		m, err := operateModelFromEnv(modelID)
		if err != nil {
			return nil, "", err
		}
		if m != nil {
			return m, modelID, nil
		}
	}
	if signedIn != nil && signedIn() {
		// No specific model → use the account's configured Codex default (id
		// "codex", empty model → no -m). An explicit codex/<model> overrides it.
		name := strings.TrimPrefix(modelID, "codex/")
		id := "codex"
		if name != "" {
			id = "codex/" + name
		}
		return operate.NewProviderModel(codexprovider.New(name), id), id, nil
	}
	if env := firstEnvModel(); env != "" {
		if m, err := operateModelFromEnv(env); err == nil && m != nil {
			return m, env, nil
		}
	}
	return nil, "", nil
}

func firstEnvModel() string {
	if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "" {
		return "anthropic/claude-sonnet-4-6"
	}
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return "openai/gpt-5.5"
	}
	return ""
}

// codexSignedIn is the production probe.
func codexSignedIn() bool {
	st, _ := (&authpkg.Codex{}).Probe(context.Background())
	return st == authpkg.StateAuthed
}
