package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	codexprovider "github.com/ArnaudGuiovanna/ouvrier/internal/provider/codex"
)

const defaultCodexAuthProbeTimeout = 2 * time.Second

// operateModelFromEnv builds the Ouvrier-owned tool-calling model transport for
// `--model provider/id`, resolving the provider from the matching API-key env
// var. It returns (nil, nil) when no model is requested.
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
//  2. a signed-in Codex subscription through the structured app-server when
//     that compatibility mode is explicitly selected;
//  3. the deterministic Ouvrier planner plus governed legacy edit/review
//     driver only when exec mode is explicitly selected. The legacy provider
//     is text-only and must never be installed into the structured tool loop;
//  4. an API-key provider from env (anthropic/openai);
//  5. nil (the cockpit shows the sign-in card; the planner remains the fallback).
//
// signedIn is injected so tests don't shell out to codex.
func resolveAgentModel(modelID, codexMode, cwd string, signedIn func() bool) (operate.AgentModel, string, error) {
	return resolveAgentModelWithFactory(modelID, codexMode, cwd, signedIn, operateModelFromEnv)
}

// resolveSelectedAgentModel keeps the selected coding-agent identity and the
// conversational transport aligned. Codex can participate directly in the
// Ouvrier-owned structured tool loop through App Server. Claude ACP owns its
// external edit turns, while Ouvrier retains the deterministic planner and
// governed tools around those turns. An explicit provider/model remains
// available for either non-Codex driver when its API key is configured.
func resolveSelectedAgentModel(agent, modelID, codexMode, cwd string, signedIn func() bool) (operate.AgentModel, string, error) {
	agent = strings.ToLower(strings.TrimSpace(agent))
	modelID = strings.TrimSpace(modelID)
	if modelID != "" && modelID != "codex" && !strings.HasPrefix(modelID, "codex/") {
		model, err := operateModelFromEnv(modelID)
		if err != nil {
			return nil, "", err
		}
		if model == nil {
			return nil, "", fmt.Errorf("configure requested operate model %q: provider returned no model", modelID)
		}
		return model, modelID, nil
	}
	if agent == "codex" {
		if strings.TrimSpace(codexMode) == "" || strings.EqualFold(strings.TrimSpace(codexMode), "auto") {
			if strings.HasPrefix(modelID, "codex/") {
				return nil, "", fmt.Errorf("%w: selecting a specific Codex model requires --codex-mode app-server; ACP uses the account default", ErrUsage)
			}
			return nil, "codex/acp", nil
		}
		return resolveAgentModel(modelID, codexMode, cwd, signedIn)
	}
	if agent == "claude" {
		return nil, "claude/acp", nil
	}
	return nil, "", nil
}

type operateModelFactory func(modelID string) (operate.AgentModel, error)

func resolveAgentModelWithFactory(modelID, codexMode, cwd string, signedIn func() bool, modelFromEnv operateModelFactory) (operate.AgentModel, string, error) {
	modelID = strings.TrimSpace(modelID)
	codexMode = strings.ToLower(strings.TrimSpace(codexMode))
	if codexMode == "" {
		codexMode = "auto"
	}
	if modelID != "" && modelID != "codex" && !strings.HasPrefix(modelID, "codex/") {
		m, err := modelFromEnv(modelID)
		if err != nil {
			return nil, "", err
		}
		if m == nil {
			return nil, "", fmt.Errorf("configure requested operate model %q: provider returned no model", modelID)
		}
		return m, modelID, nil
	}
	if signedIn != nil && signedIn() {
		// No specific model → use the account's configured Codex default (id
		// "codex", empty model → no -m). An explicit codex/<model> overrides it.
		name := ""
		if strings.HasPrefix(modelID, "codex/") {
			name = strings.TrimPrefix(modelID, "codex/")
		}
		id := "codex"
		if name != "" {
			id = "codex/" + name
		}
		if codexMode != "exec" {
			p, err := newCodexAgentProvider(codexMode, name, cwd)
			if err != nil {
				return nil, "", err
			}
			return operate.NewProviderModel(p, id), id, nil
		}
		if modelID == "codex" || strings.HasPrefix(modelID, "codex/") {
			return nil, "", fmt.Errorf("%w: --model %q needs the structured tool transport; use --codex-mode app-server", ErrUsage, modelID)
		}
		// codex exec cannot return provider.ToolCall values. Leave Model nil so
		// free-form prompts use Ouvrier's deterministic governed plan; the
		// Codex Driver remains the implementation behind patch/review/fix.
		return nil, id, nil
	}
	if strings.HasPrefix(modelID, "codex/") || modelID == "codex" || codexMode == "app-server" || codexMode == "exec" {
		return nil, "", fmt.Errorf("%w: the requested Codex transport needs an available saved Codex session", ErrUsage)
	}
	if env := firstEnvModel(); env != "" {
		m, err := modelFromEnv(env)
		if err != nil {
			return nil, "", fmt.Errorf("configure automatic operate model %q from environment: %w", env, err)
		}
		if m == nil {
			return nil, "", fmt.Errorf("configure automatic operate model %q from environment: provider returned no model", env)
		}
		return m, env, nil
	}
	return nil, "", nil
}

func newCodexAgentProvider(mode, model, cwd string) (provider.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "exec":
		return codexprovider.New(model), nil
	case "", "auto", "app-server":
		abs := strings.TrimSpace(cwd)
		if abs != "" {
			var err error
			abs, err = filepath.Abs(abs)
			if err != nil {
				return nil, fmt.Errorf("%w: resolve Codex app-server workspace: %v", ErrUsage, err)
			}
		}
		return codexprovider.NewAppServer(model, abs), nil
	default:
		return nil, fmt.Errorf("%w: --codex-mode must be auto, exec, or app-server", ErrUsage)
	}
}

func firstEnvModel() string {
	if providerEnvironmentConfigured("ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL") {
		return "anthropic/claude-sonnet-4-6"
	}
	if providerEnvironmentConfigured("OPENAI_API_KEY", "OPENAI_BASE_URL") {
		return "openai/gpt-5.5"
	}
	return ""
}

func providerEnvironmentConfigured(names ...string) bool {
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// codexSignedIn is the production probe.
func codexSignedIn() bool {
	return codexSignedInWithRunner(nil, defaultCodexAuthProbeTimeout)
}

func codexSignedInWithRunner(runner authpkg.Runner, timeout time.Duration) bool {
	if timeout <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	st, _ := (&authpkg.Codex{Runner: runner}).Probe(ctx)
	return st == authpkg.StateAuthed
}
