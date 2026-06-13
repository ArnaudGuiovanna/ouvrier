package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetJSONSendsBearerWhenTokenSet(t *testing.T) {
	const token = "abc-123"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer srv.Close()

	c := NewClient(nil, srv.URL, token)
	var out map[string]any
	if err := c.GetJSON(context.Background(), "/admin/status", &out); err != nil {
		t.Fatalf("GetJSON error = %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want Bearer %s", gotAuth, token)
	}
	if out["status"] != "ok" {
		t.Fatalf("decoded status = %v, want ok", out["status"])
	}
}

func TestGetJSONOmitsBearerWhenTokenEmpty(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()

	// Tunnel mode: the transport injects the token, the Client must not.
	c := NewClient(srv.Client(), srv.URL, "")
	if err := c.GetJSON(context.Background(), "/admin/status", nil); err != nil {
		t.Fatalf("GetJSON error = %v", err)
	}
	if hadAuth {
		t.Fatal("Client set its own Authorization header in tunnel mode")
	}
}

func TestGetJSONReturnsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"admin_token_required"}`))
	}))
	defer srv.Close()

	c := NewClient(nil, srv.URL, "")
	err := c.GetJSON(context.Background(), "/admin/status", nil)
	var httpErr HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want HTTPError", err)
	}
	if httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", httpErr.Status)
	}
}

func TestRedactURLHidesUserinfo(t *testing.T) {
	got := RedactURL("http://user:pass@example.com/admin/status")
	if strings.Contains(got, "user:pass") || !strings.Contains(got, "***@") {
		t.Fatalf("RedactURL = %q, want masked userinfo", got)
	}
}
