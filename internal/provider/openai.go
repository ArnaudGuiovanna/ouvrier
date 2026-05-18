package provider

import (
	"context"
	"net/http"
)

const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

type OpenAIConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type OpenAI struct {
	compat *openAICompatProvider
}

func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	compat, err := newOpenAICompatProvider(openAICompatConfig{
		name:           "openai",
		defaultBaseURL: DefaultOpenAIBaseURL,
		apiKey:         cfg.APIKey,
		baseURL:        cfg.BaseURL,
		apiKeyRequired: true,
		httpClient:     cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &OpenAI{compat: compat}, nil
}

func (p *OpenAI) Name() string {
	return p.compat.Name()
}

func (p *OpenAI) BaseURL() string {
	return p.compat.BaseURL()
}

func (p *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	return p.compat.Complete(ctx, req)
}
