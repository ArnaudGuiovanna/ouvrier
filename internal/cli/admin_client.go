package cli

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
	defaultAdminURL     = "http://127.0.0.1:8080"
	defaultAdminTimeout = 10 * time.Second
)

// adminClient performs authenticated GET requests against a worker's
// /admin/* endpoints.
type adminClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newAdminClient(baseURL, token string) *adminClient {
	url := strings.TrimSpace(baseURL)
	if url == "" {
		url = defaultAdminURL
	}
	url = strings.TrimRight(url, "/")
	return &adminClient{
		baseURL: url,
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: defaultAdminTimeout},
	}
}

// getJSON issues GET against the supplied path (with optional raw query
// string) and decodes the JSON response into out. The Authorization header
// is sent when a token is configured; nothing about the header is logged or
// returned to callers.
func (c *adminClient) getJSON(ctx context.Context, path string, out any) error {
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
		return fmt.Errorf("call %s: %w", redactURL(endpoint), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return adminHTTPError{
			Status: resp.StatusCode,
			URL:    redactURL(endpoint),
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

type adminHTTPError struct {
	Status int
	URL    string
	Body   string
}

func (e adminHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("admin request %s returned HTTP %d", e.URL, e.Status)
	}
	return fmt.Sprintf("admin request %s returned HTTP %d: %s", e.URL, e.Status, e.Body)
}

// reorderFlagsFirst returns args with all -flag/--flag tokens (and their
// values) moved ahead of any positional arguments. This lets commands that
// take a positional argument (e.g. trace <exec-id>) accept --url and
// --token in any order with the standard library's flag package, which
// otherwise stops parsing at the first non-flag token.
func reorderFlagsFirst(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))

	knownBool := map[string]struct{}{} // none of our commands use bool flags positionally today.
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			// Pass-through delimiter; preserve order from here.
			positional = append(positional, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// If the flag doesn't already embed =value and isn't known to be
			// boolean, the next token is its value.
			if !strings.Contains(a, "=") {
				if _, isBool := knownBool[strings.TrimLeft(a, "-")]; !isBool && i+1 < len(args) {
					flags = append(flags, args[i+1])
					i++
				}
			}
		} else {
			positional = append(positional, a)
		}
		i++
	}
	return append(flags, positional...)
}

// redactURL strips userinfo from a URL so we never leak credentials embedded
// in baseURL via error messages.
func redactURL(raw string) string {
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
