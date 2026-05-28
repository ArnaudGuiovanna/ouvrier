package provider

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// DefaultAzureOpenAIAPIVersion is the api-version query value used when the
// caller does not provide one.
const DefaultAzureOpenAIAPIVersion = "2024-06-01"

type AzureOpenAIConfig struct {
	APIKey     string
	BaseURL    string
	APIVersion string
	HTTPClient *http.Client
}

// AzureOpenAI is an OpenAI-compatible adapter for the Azure OpenAI Service.
//
// Azure differs from vanilla OpenAI in three ways handled here:
//   - it authenticates with an "api-key" header instead of "Authorization: Bearer"
//   - it uses a deployment-based path "/openai/deployments/<deployment>/chat/completions"
//   - it requires an "api-version" query parameter
//
// The model reference "azure/<deployment>" names the Azure deployment.
type AzureOpenAI struct {
	compat     *openAICompatProvider
	apiVersion string
}

func NewAzureOpenAI(cfg AzureOpenAIConfig) (*AzureOpenAI, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("azure base URL is required")
	}
	apiVersion := strings.TrimSpace(cfg.APIVersion)
	if apiVersion == "" {
		apiVersion = DefaultAzureOpenAIAPIVersion
	}

	compat, err := newOpenAICompatProvider(openAICompatConfig{
		name:           "azure",
		apiKey:         cfg.APIKey,
		baseURL:        cfg.BaseURL,
		apiKeyRequired: true,
		httpClient:     cfg.HTTPClient,
		authStyle:      authAPIKeyHeader,
		urlBuilder:     azureURLBuilder(apiVersion),
	})
	if err != nil {
		return nil, err
	}
	return &AzureOpenAI{compat: compat, apiVersion: apiVersion}, nil
}

func azureURLBuilder(apiVersion string) compatURLBuilder {
	return func(baseURL, deployment string) string {
		path := baseURL + "/openai/deployments/" + url.PathEscape(deployment) + "/chat/completions"
		query := url.Values{"api-version": []string{apiVersion}}
		return path + "?" + query.Encode()
	}
}

func (p *AzureOpenAI) Name() string {
	return p.compat.Name()
}

func (p *AzureOpenAI) BaseURL() string {
	return p.compat.BaseURL()
}

func (p *AzureOpenAI) APIVersion() string {
	return p.apiVersion
}

func (p *AzureOpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	return p.compat.Complete(ctx, req)
}
