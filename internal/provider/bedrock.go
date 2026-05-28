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

const bedrockService = "bedrock"

// httpDoer is the minimal HTTP boundary used by the Bedrock adapter so the
// SigV4-signed request can be exercised against a fake in unit tests.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type BedrockConfig struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
	// BaseURL overrides the default bedrock-runtime endpoint (mainly for tests).
	BaseURL string
	// Doer overrides the HTTP client (defaults to http.DefaultClient).
	Doer httpDoer
	// now overrides the signing clock (tests only).
	now func() time.Time
}

// Bedrock is the Amazon Bedrock Converse API adapter. Bedrock is not
// OpenAI-compatible: requests use the Converse schema and are authenticated
// with AWS Signature Version 4. The model reference "bedrock/<modelId>" names
// the Bedrock model identifier.
type Bedrock struct {
	creds   awsCredentials
	region  string
	baseURL string
	doer    httpDoer
	now     func() time.Time
}

func NewBedrock(cfg BedrockConfig) (*Bedrock, error) {
	accessKey := strings.TrimSpace(cfg.AccessKeyID)
	secret := strings.TrimSpace(cfg.SecretAccessKey)
	region := strings.TrimSpace(cfg.Region)
	if accessKey == "" {
		return nil, errors.New("bedrock access key ID is required")
	}
	if secret == "" {
		return nil, errors.New("bedrock secret access key is required")
	}
	if region == "" {
		return nil, errors.New("bedrock region is required")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region)
	}

	doer := cfg.Doer
	if doer == nil {
		doer = http.DefaultClient
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}

	return &Bedrock{
		creds: awsCredentials{
			AccessKeyID:     accessKey,
			SecretAccessKey: secret,
			SessionToken:    strings.TrimSpace(cfg.SessionToken),
		},
		region:  region,
		baseURL: baseURL,
		doer:    doer,
		now:     now,
	}, nil
}

func (p *Bedrock) Name() string { return "bedrock" }

func (p *Bedrock) BaseURL() string { return p.baseURL }

func (p *Bedrock) Complete(ctx context.Context, req Request) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	ref, _ := ParseModelID(req.Model)
	if ref.Provider != p.Name() {
		return Response{}, fmt.Errorf("%s provider cannot run model %q", p.Name(), req.Model)
	}
	started := time.Now()

	body, err := buildBedrockConverseRequest(req)
	if err != nil {
		return Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("marshal bedrock request: %w", err)
	}

	endpoint := p.baseURL + "/model/" + ref.Name + "/converse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	signSigV4(httpReq, payload, p.creds, p.region, bedrockService, p.now())

	httpResp, err := p.doer.Do(httpReq)
	if err != nil {
		return Response{}, transportError(p.Name(), err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<10))
		return Response{}, statusError(p.Name(), httpResp.Status, httpResp.StatusCode, string(detail))
	}

	var decoded bedrockConverseResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&decoded); err != nil {
		return Response{}, fmt.Errorf("decode bedrock response: %w", err)
	}
	resp, err := decoded.toProviderResponse()
	if err != nil {
		return Response{}, fmt.Errorf("decode bedrock response: %w", err)
	}
	return attachResponseMetadata(resp, p.Name(), req.Model, started, req, promptCacheUnsupported), nil
}
