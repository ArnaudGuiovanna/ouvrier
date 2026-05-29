package provider

import (
	"context"
	"net/http"
)

const DefaultGroqBaseURL = "https://api.groq.com/openai/v1"

type GroqConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Groq struct {
	compat *openAICompatProvider
}

func NewGroq(cfg GroqConfig) (*Groq, error) {
	compat, err := newOpenAICompatProvider(openAICompatConfig{
		name:           "groq",
		defaultBaseURL: DefaultGroqBaseURL,
		apiKey:         cfg.APIKey,
		baseURL:        cfg.BaseURL,
		apiKeyRequired: true,
		httpClient:     cfg.HTTPClient,
		authStyle:      authBearer,
	})
	if err != nil {
		return nil, err
	}
	return &Groq{compat: compat}, nil
}

func (p *Groq) Name() string {
	return p.compat.Name()
}

func (p *Groq) BaseURL() string {
	return p.compat.BaseURL()
}

func (p *Groq) Complete(ctx context.Context, req Request) (Response, error) {
	return p.compat.Complete(ctx, req)
}
