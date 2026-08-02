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
	"time"
)

// authStyle selects how the API key is presented to the upstream provider.
type authStyle int

const (
	// authBearer sends "Authorization: Bearer <key>" (OpenAI default).
	authBearer authStyle = iota
	// authAPIKeyHeader sends "api-key: <key>" (Azure OpenAI style).
	authAPIKeyHeader
)

// compatURLBuilder shapes the chat-completions endpoint URL for a request.
// model is the unprefixed model/deployment name. When nil, the default
// "<baseURL>/chat/completions" shaping is used.
type compatURLBuilder func(baseURL, model string) string

type openAICompatConfig struct {
	name           string
	defaultBaseURL string
	apiKey         string
	baseURL        string
	apiKeyRequired bool
	httpClient     *http.Client
	authStyle      authStyle
	urlBuilder     compatURLBuilder
}

type openAICompatProvider struct {
	name       string
	apiKey     string
	baseURL    string
	httpClient *http.Client
	authStyle  authStyle
	urlBuilder compatURLBuilder
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
		authStyle:  cfg.authStyle,
		urlBuilder: cfg.urlBuilder,
	}, nil
}

func (p *openAICompatProvider) requestURL(model string) string {
	if p.urlBuilder != nil {
		return p.urlBuilder(p.baseURL, model)
	}
	return p.baseURL + "/chat/completions"
}

func (p *openAICompatProvider) setAuthHeader(header http.Header) {
	if p.apiKey == "" {
		return
	}
	if p.authStyle == authAPIKeyHeader {
		header.Set("api-key", p.apiKey)
		return
	}
	// authBearer (the default) sends a standard bearer token.
	header.Set("Authorization", "Bearer "+p.apiKey)
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
	started := time.Now()

	body, err := buildOpenAICompatRequest(ref.Name, req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal %s request: %w", p.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.requestURL(ref.Name), bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.setAuthHeader(httpReq.Header)

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, transportError(p.name, err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, statusError(p.name, httpResp.Status, httpResp.StatusCode, string(body))
	}

	var decoded openAICompatResponse
	if err := decodeBoundedProviderJSON(httpResp.Body, &decoded); err != nil {
		return Response{}, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	resp, err := decoded.toProviderResponse()
	if err != nil {
		return Response{}, fmt.Errorf("decode %s response: %w", p.name, err)
	}
	return attachResponseMetadata(resp, p.name, req.Model, started, req, promptCacheUnsupported), nil
}
