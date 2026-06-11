package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

func TestRunStatusFetchesAndPrintsAdminCounters(t *testing.T) {
	const token = "secret-admin-token"
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/admin/status" {
			t.Errorf("path = %q, want /admin/status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":                   "ok",
			"sessions":                 3,
			"executions":               7,
			"events":                   42,
			"by_status":                map[string]int{"completed": 5, "running": 2},
			"schema_validation_passed": 5,
			"schema_validation_failed": 1,
			"schema_violations":        1,
			"llm_calls":                10,
			"llm_failures":             0,
			"input_tokens":             123,
			"output_tokens":            456,
			"cost_usd":                 0.0125,
			"average_latency_ms":       250.0,
			"tool_calls":               20,
			"tool_calls_completed":     19,
			"tool_failures":            1,
			"permission_allowed":       30,
			"permission_denied":        0,
			"budget_exceeded":          0,
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"status", "--url", srv.URL, "--token", token})
	if err != nil {
		t.Fatalf("Run(status) error = %v", err)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
	if receivedAuth != "Bearer "+token {
		t.Fatalf("server Authorization = %q, want Bearer %s", receivedAuth, token)
	}

	stdout := out.String()
	if strings.Contains(stdout, token) {
		t.Fatalf("status output leaked admin token in:\n%s", stdout)
	}
	for _, want := range []string{
		"status:            ok",
		"sessions:          3",
		"executions:        7",
		"by_status:         completed=5, running=2",
		"validation_passed: 5",
		"violations:        1",
		"calls:             10",
		"input_tokens:      123",
		"cost_usd:          0.0125",
		"avg_latency_ms:    250",
		"calls:             20",
		"perm_allowed:      30",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status output missing %q in:\n%s", want, stdout)
		}
	}
}

func TestRunStatusPropagatesAdminError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"admin_token_required"}`))
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"status", "--url", srv.URL})
	if err == nil {
		t.Fatal("Run(status) error = nil, want HTTP error")
	}
	var httpErr adminHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want adminHTTPError", err)
	}
	if httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", httpErr.Status)
	}
}

func TestRedactURLHidesUserinfo(t *testing.T) {
	got := redactURL("http://user:pass@example.com/admin/status")
	if !strings.Contains(got, "***@") {
		t.Fatalf("redactURL = %q, want masked userinfo", got)
	}
	if strings.Contains(got, "user:pass") {
		t.Fatalf("redactURL still contains credentials: %q", got)
	}
}

func TestResolveAdminTokenRejectsLegacyEnv(t *testing.T) {
	t.Setenv(envnames.AdminToken, "")
	t.Setenv(envnames.LegacyAdminToken, "old-secret")
	_, err := resolveAdminToken("")
	if err == nil || !strings.Contains(err.Error(), envnames.AdminToken) {
		t.Fatalf("expected migration error naming %s, got %v", envnames.AdminToken, err)
	}
}

func TestResolveAdminTokenPrecedence(t *testing.T) {
	t.Setenv(envnames.LegacyAdminToken, "old-secret")
	if got, err := resolveAdminToken("flag-token"); err != nil || got != "flag-token" {
		t.Fatalf("flag must win: got %q, %v", got, err)
	}
	t.Setenv(envnames.AdminToken, "new-secret")
	if got, err := resolveAdminToken(""); err != nil || got != "new-secret" {
		t.Fatalf("%s must win over legacy: got %q, %v", envnames.AdminToken, got, err)
	}
}
