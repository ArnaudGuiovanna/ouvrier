package provider

import (
	"context"
	"net/http"
)

const DefaultVLLMBaseURL = "http://localhost:8000/v1"

type VLLMConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type VLLM struct {
	compat *openAICompatProvider
}

func NewVLLM(cfg VLLMConfig) (*VLLM, error) {
	compat, err := newOpenAICompatProvider(openAICompatConfig{
		name:           "vllm",
		defaultBaseURL: DefaultVLLMBaseURL,
		apiKey:         cfg.APIKey,
		baseURL:        cfg.BaseURL,
		apiKeyRequired: false,
		httpClient:     cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &VLLM{compat: compat}, nil
}

func (p *VLLM) Name() string {
	return p.compat.Name()
}

func (p *VLLM) BaseURL() string {
	return p.compat.BaseURL()
}

func (p *VLLM) Complete(ctx context.Context, req Request) (Response, error) {
	return p.compat.Complete(ctx, req)
}
