package ovr

import (
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func providerRegistryFromEnv() (*provider.Registry, error) {
	var providers []provider.Provider

	if key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); key != "" {
		p, err := provider.NewAnthropic(provider.AnthropicConfig{
			APIKey:  key,
			BaseURL: os.Getenv("ANTHROPIC_BASE_URL"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		p, err := provider.NewOpenAI(provider.OpenAIConfig{
			APIKey:  key,
			BaseURL: os.Getenv("OPENAI_BASE_URL"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")); key != "" {
		p, err := provider.NewMistral(provider.MistralConfig{
			APIKey:  key,
			BaseURL: os.Getenv("MISTRAL_BASE_URL"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); key != "" {
		p, err := provider.NewGemini(provider.GeminiConfig{
			APIKey:  key,
			BaseURL: os.Getenv("GEMINI_BASE_URL"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}

	if key := strings.TrimSpace(os.Getenv("GROQ_API_KEY")); key != "" {
		p, err := provider.NewGroq(provider.GroqConfig{
			APIKey:  key,
			BaseURL: os.Getenv("GROQ_BASE_URL"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); key != "" {
		p, err := provider.NewDeepSeek(provider.DeepSeekConfig{
			APIKey:  key,
			BaseURL: os.Getenv("DEEPSEEK_BASE_URL"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("AZURE_OPENAI_API_KEY")); key != "" {
		p, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{
			APIKey:     key,
			BaseURL:    os.Getenv("AZURE_OPENAI_BASE_URL"),
			APIVersion: os.Getenv("AZURE_OPENAI_API_VERSION"),
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	if key := strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID")); key != "" {
		if region := strings.TrimSpace(os.Getenv("AWS_REGION")); region != "" {
			p, err := provider.NewBedrock(provider.BedrockConfig{
				AccessKeyID:     key,
				SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
				SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
				Region:          region,
				BaseURL:         os.Getenv("AWS_BEDROCK_BASE_URL"),
			})
			if err != nil {
				return nil, err
			}
			providers = append(providers, p)
		}
	}

	providers = append(providers,
		provider.NewOllama(provider.OllamaConfig{BaseURL: os.Getenv("OLLAMA_BASE_URL")}),
	)
	vllm, err := provider.NewVLLM(provider.VLLMConfig{
		APIKey:  os.Getenv("VLLM_API_KEY"),
		BaseURL: os.Getenv("VLLM_BASE_URL"),
	})
	if err != nil {
		return nil, err
	}
	providers = append(providers, vllm)

	return provider.NewRegistry(providers...)
}

func adminTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(envnames.AdminToken))
}
