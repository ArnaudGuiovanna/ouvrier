// Package adminapi is the shared client for a worker's authenticated
// /admin/* endpoints. It is deliberately free of any dependency on
// internal/cli (the CLI and the future web console both consume it).
//
// Two construction paths are supported, mirroring the two ways the CLI reaches
// a worker:
//
//   - Local --url mode: NewClient(httpClient, baseURL, token). The client adds
//     Authorization: Bearer <token> on every request, exactly as before.
//   - Fleet --worker/--all mode: the httpClient's Transport is a tunnel
//     manager's per-worker RoundTripper, which injects the in-memory fetched
//     token itself. Pass an empty token so the client does NOT set its own
//     header and never sees the secret.
package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultAdminURL is the loopback base URL used when none is supplied.
	DefaultAdminURL = "http://127.0.0.1:8080"
	// DefaultTimeout bounds a single admin request in local --url mode.
	DefaultTimeout = 10 * time.Second
)

// Client performs authenticated GET requests against a worker's /admin/*
// endpoints. It wraps an *http.Client and a base URL; the optional token is
// sent as a bearer header only when non-empty.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a Client over httpClient targeting baseURL. When token is
// empty the Authorization header is left untouched (tunnel mode: the manager's
// transport injects the token). When httpClient is nil a default client with
// DefaultTimeout is used. An empty baseURL falls back to DefaultAdminURL.
func NewClient(httpClient *http.Client, baseURL, token string) *Client {
	url := strings.TrimSpace(baseURL)
	if url == "" {
		url = DefaultAdminURL
	}
	url = strings.TrimRight(url, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		baseURL: url,
		token:   strings.TrimSpace(token),
		http:    httpClient,
	}
}

// BaseURL reports the normalized base URL the client targets.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// GetJSON issues GET against the supplied path (with optional raw query
// string) and decodes the JSON response into out. The Authorization header is
// sent when a token is configured; nothing about the header is logged or
// returned to callers. A non-2xx response yields an HTTPError.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	if c == nil {
		return fmt.Errorf("admin client is nil")
	}
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build admin request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", RedactURL(endpoint), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return HTTPError{
			Status: resp.StatusCode,
			URL:    RedactURL(endpoint),
			Body:   strings.TrimSpace(string(body)),
		}
	}

	if out == nil {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode admin response: %w", err)
	}
	return nil
}

// HTTPError is returned for non-2xx admin responses.
type HTTPError struct {
	Status int
	URL    string
	Body   string
}

func (e HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("admin request %s returned HTTP %d", e.URL, e.Status)
	}
	return fmt.Sprintf("admin request %s returned HTTP %d: %s", e.URL, e.Status, e.Body)
}

// RedactURL strips userinfo from a URL so credentials embedded in a base URL
// never leak via error messages.
func RedactURL(raw string) string {
	at := strings.Index(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := strings.Index(raw, "://")
	if scheme < 0 || scheme > at {
		return raw
	}
	return raw[:scheme+3] + "***@" + raw[at+1:]
}
