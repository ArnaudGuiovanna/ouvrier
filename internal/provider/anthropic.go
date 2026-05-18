package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const DefaultAnthropicBaseURL = "https://api.anthropic.com"
const anthropicVersion = "2023-06-01"
const defaultAnthropicMaxTokens = 4096

type AnthropicConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Anthropic struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
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
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Anthropic{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: httpClient,
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
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != a.Name() {
		return Response{}, fmt.Errorf("anthropic provider cannot run model %q", req.Model)
	}

	body, err := buildAnthropicRequest(ref.Name, req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal anthropic request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, fmt.Errorf("anthropic %s: %s", httpResp.Status, strings.TrimSpace(string(body)))
	}

	var decoded anthropicResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("decode anthropic response: %w", err)
	}
	return decoded.toProviderResponse()
}
