package provider

import (
	"context"
	"net/http"
)

const DefaultDeepSeekBaseURL = "https://api.deepseek.com/v1"

type DeepSeekConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type DeepSeek struct {
	compat *openAICompatProvider
}

func NewDeepSeek(cfg DeepSeekConfig) (*DeepSeek, error) {
	compat, err := newOpenAICompatProvider(openAICompatConfig{
		name:           "deepseek",
		defaultBaseURL: DefaultDeepSeekBaseURL,
		apiKey:         cfg.APIKey,
		baseURL:        cfg.BaseURL,
		apiKeyRequired: true,
		httpClient:     cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &DeepSeek{compat: compat}, nil
}

func (p *DeepSeek) Name() string {
	return p.compat.Name()
}

func (p *DeepSeek) BaseURL() string {
	return p.compat.BaseURL()
}

func (p *DeepSeek) Complete(ctx context.Context, req Request) (Response, error) {
	return p.compat.Complete(ctx, req)
}
