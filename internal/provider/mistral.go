package provider

import (
	"context"
	"net/http"
)

const DefaultMistralBaseURL = "https://api.mistral.ai/v1"

type MistralConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Mistral struct {
	compat *openAICompatProvider
}

func NewMistral(cfg MistralConfig) (*Mistral, error) {
	compat, err := newOpenAICompatProvider(openAICompatConfig{
		name:           "mistral",
		defaultBaseURL: DefaultMistralBaseURL,
		apiKey:         cfg.APIKey,
		baseURL:        cfg.BaseURL,
		apiKeyRequired: true,
		httpClient:     cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &Mistral{compat: compat}, nil
}

func (p *Mistral) Name() string {
	return p.compat.Name()
}

func (p *Mistral) BaseURL() string {
	return p.compat.BaseURL()
}

func (p *Mistral) Complete(ctx context.Context, req Request) (Response, error) {
	return p.compat.Complete(ctx, req)
}
