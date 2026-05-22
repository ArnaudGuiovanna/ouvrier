package mcpclient

import (
	"net/http"
	"strings"
	"testing"
)

func TestEnvPrefixNormalizesServerName(t *testing.T) {
	if got := EnvPrefix("moodle-mcp"); got != "MOODLE_MCP" {
		t.Fatalf("EnvPrefix = %q, want MOODLE_MCP", got)
	}
}

func TestEnvConnectorRejectsMissingURL(t *testing.T) {
	connector := NewEnvConnector(WithEnvGetter(func(string) string { return "" }))

	_, err := connector.transport("moodle-mcp")
	if err == nil {
		t.Fatal("transport returned nil error")
	}
}

func TestEnvConnectorRejectsInvalidURLWithoutLeakingValue(t *testing.T) {
	secretURL := "file:///tmp/secret-token"
	connector := NewEnvConnector(WithEnvGetter(func(key string) string {
		if key == "MOODLE_MCP_URL" {
			return secretURL
		}
		return ""
	}))

	_, err := connector.transport("moodle-mcp")
	if err == nil {
		t.Fatal("transport returned nil error")
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error leaked endpoint value: %v", err)
	}
}

func TestAuthenticatedHTTPClientAddsBearerTokenWithoutMutatingOriginalRequest(t *testing.T) {
	var gotAuth string
	client := NewEnvConnector(WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
		}),
	})).authenticatedHTTPClient("secret-token")
	req, err := http.NewRequest(http.MethodGet, "https://mcp.example.test/sse", nil)
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	_ = resp.Body.Close()
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatalf("original request Authorization = %q, want empty", req.Header.Get("Authorization"))
	}
}

func TestAuthenticatedHTTPClientDoesNotOverrideExistingAuthorization(t *testing.T) {
	var gotAuth string
	client := NewEnvConnector(WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
		}),
	})).authenticatedHTTPClient("secret-token")
	req, err := http.NewRequest(http.MethodGet, "https://mcp.example.test/sse", nil)
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	req.Header.Set("Authorization", "Bearer caller-token")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	_ = resp.Body.Close()
	if gotAuth != "Bearer caller-token" {
		t.Fatalf("Authorization = %q, want caller token", gotAuth)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
