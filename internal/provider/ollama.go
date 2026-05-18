package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const DefaultOllamaBaseURL = "http://localhost:11434"

type OllamaConfig struct {
	BaseURL    string
	HTTPClient *http.Client
}

type Ollama struct {
	baseURL    string
	httpClient *http.Client
}

func NewOllama(cfg OllamaConfig) *Ollama {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultOllamaBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Ollama{baseURL: baseURL, httpClient: client}
}

func (p *Ollama) Name() string {
	return "ollama"
}

func (p *Ollama) BaseURL() string {
	return p.baseURL
}

func (p *Ollama) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != p.Name() {
		return Response{}, fmt.Errorf("ollama provider cannot run model %q", req.Model)
	}

	body, err := buildOllamaNativeRequest(ref.Name, req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal ollama request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, fmt.Errorf("ollama %s: %s", httpResp.Status, strings.TrimSpace(string(body)))
	}

	var decoded ollamaNativeResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("decode ollama response: %w", err)
	}
	return decoded.toProviderResponse()
}
