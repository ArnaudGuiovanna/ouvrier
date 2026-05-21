package mcpclient

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type EnvConnector struct {
	connector  *Connector
	getenv     func(string) string
	httpClient *http.Client
}

type EnvOption func(*EnvConnector)

func NewEnvConnector(options ...EnvOption) *EnvConnector {
	connector := &EnvConnector{
		connector: NewConnector(),
		getenv:    os.Getenv,
	}
	for _, option := range options {
		if option != nil {
			option(connector)
		}
	}
	if connector.getenv == nil {
		connector.getenv = os.Getenv
	}
	if connector.connector == nil {
		connector.connector = NewConnector()
	}
	return connector
}

func WithEnvGetter(getenv func(string) string) EnvOption {
	return func(connector *EnvConnector) {
		connector.getenv = getenv
	}
}

func WithHTTPClient(client *http.Client) EnvOption {
	return func(connector *EnvConnector) {
		connector.httpClient = client
	}
}

func (c *EnvConnector) Connect(ctx context.Context, serverName string) (*Session, error) {
	transport, err := c.transport(serverName)
	if err != nil {
		return nil, err
	}
	return c.connector.Connect(ctx, Server{Name: serverName, Transport: transport})
}

func (c *EnvConnector) transport(serverName string) (mcp.Transport, error) {
	prefix := EnvPrefix(serverName)
	endpoint := strings.TrimSpace(c.getenv(prefix + "_URL"))
	if endpoint == "" {
		return nil, fmt.Errorf("%w: %s_URL is required", ErrInvalidServer, prefix)
	}
	client := c.authenticatedHTTPClient(strings.TrimSpace(c.getenv(prefix + "_TOKEN")))

	switch strings.ToLower(strings.TrimSpace(c.getenv(prefix + "_TRANSPORT"))) {
	case "", "streamable", "streamable_http", "http":
		return &mcp.StreamableClientTransport{
			Endpoint:             endpoint,
			HTTPClient:           client,
			DisableStandaloneSSE: true,
		}, nil
	case "sse":
		return &mcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: client}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported %s_TRANSPORT", ErrInvalidServer, prefix)
	}
}

func EnvPrefix(serverName string) string {
	return strings.ToUpper(envName(serverName, "MCP"))
}

func envName(name, fallback string) string {
	name = strings.TrimSpace(name)
	var out strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			out.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z':
			out.WriteRune(r)
		case r >= '0' && r <= '9':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	cleaned := strings.Trim(out.String(), "_")
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func (c *EnvConnector) authenticatedHTTPClient(token string) *http.Client {
	if token == "" {
		return c.httpClient
	}
	base := http.DefaultTransport
	if c.httpClient != nil && c.httpClient.Transport != nil {
		base = c.httpClient.Transport
	}
	client := *http.DefaultClient
	if c.httpClient != nil {
		client = *c.httpClient
	}
	client.Transport = bearerRoundTripper{token: token, next: base}
	return &client
}

type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	next := rt.next
	if next == nil {
		next = http.DefaultTransport
	}
	cloned := req.Clone(req.Context())
	if cloned.Header.Get("Authorization") == "" {
		cloned.Header.Set("Authorization", "Bearer "+rt.token)
	}
	return next.RoundTrip(cloned)
}
