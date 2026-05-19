package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const DefaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

type GeminiConfig struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

type Gemini struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewGemini(cfg GeminiConfig) (*Gemini, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("gemini API key is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultGeminiBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Gemini{apiKey: apiKey, baseURL: baseURL, httpClient: client}, nil
}

func (p *Gemini) Name() string {
	return "gemini"
}

func (p *Gemini) BaseURL() string {
	return p.baseURL
}

func (p *Gemini) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != p.Name() {
		return Response{}, fmt.Errorf("gemini provider cannot run model %q", req.Model)
	}

	body, err := buildGeminiRequest(req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal gemini request: %w", err)
	}

	endpoint := p.baseURL + "/v1beta/models/" + url.PathEscape(ref.Name) + ":generateContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.apiKey)
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, transportError(p.Name(), err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, statusError(p.Name(), httpResp.Status, httpResp.StatusCode, string(body))
	}

	var decoded geminiResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("decode gemini response: %w", err)
	}
	return decoded.toProviderResponse()
}
