package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const DefaultAnthropicBaseURL = "https://api.anthropic.com"

var ErrNotImplemented = errors.New("provider integration not implemented")

type AnthropicConfig struct {
	APIKey  string
	BaseURL string
}

type Anthropic struct {
	apiKey  string
	baseURL string
}

func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, errors.New("anthropic API key is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultAnthropicBaseURL
	}
	return &Anthropic{
		apiKey:  apiKey,
		baseURL: baseURL,
	}, nil
}

func (a *Anthropic) Name() string {
	return "anthropic"
}

func (a *Anthropic) BaseURL() string {
	return a.baseURL
}

func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	if ref, _ := ParseModelID(req.Model); ref.Provider != a.Name() {
		return Response{}, fmt.Errorf("anthropic provider cannot run model %q", req.Model)
	}
	return Response{}, ErrNotImplemented
}
