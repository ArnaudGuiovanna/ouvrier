package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
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
