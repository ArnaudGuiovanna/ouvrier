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

type openAICompatConfig struct {
	name           string
	defaultBaseURL string
	apiKey         string
	baseURL        string
	apiKeyRequired bool
	httpClient     *http.Client
}

type openAICompatProvider struct {
	name       string
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func newOpenAICompatProvider(cfg openAICompatConfig) (*openAICompatProvider, error) {
	name := strings.TrimSpace(cfg.name)
	if name == "" {
		return nil, errors.New("provider name is required")
	}

	apiKey := strings.TrimSpace(cfg.apiKey)
	if cfg.apiKeyRequired && apiKey == "" {
		return nil, fmt.Errorf("%s API key is required", name)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.baseURL), "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(strings.TrimSpace(cfg.defaultBaseURL), "/")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("%s base URL is required", name)
	}

	httpClient := cfg.httpClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &openAICompatProvider{
		name:       name,
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: httpClient,
	}, nil
}

func (p *openAICompatProvider) Name() string {
	return p.name
}

func (p *openAICompatProvider) BaseURL() string {
	return p.baseURL
}

func (p *openAICompatProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != p.name {
		return Response{}, fmt.Errorf("%s provider cannot run model %q", p.name, req.Model)
	}

	body, err := buildOpenAICompatRequest(ref.Name, req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal %s request: %w", p.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			return Response{}, fmt.Errorf("%s %s", p.name, httpResp.Status)
		}
		return Response{}, fmt.Errorf("%s %s: %s", p.name, httpResp.Status, detail)
	}

	var decoded openAICompatResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	resp, err := decoded.toProviderResponse()
	if err != nil {
		return Response{}, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	return resp, nil
}
