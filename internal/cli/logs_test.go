package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunLogsListsTraces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/traces" {
			t.Errorf("path = %q, want /admin/traces", r.URL.Path)
		}
		if got := r.URL.Query().Get("last"); got != "3" {
			t.Errorf("last = %q, want 3", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"traces": []map[string]any{
				{
					"trace_key":          "exec:abc-1",
					"exec_id":            "abc-1",
					"events":             12,
					"last_kind":          "pipe.completed",
					"average_latency_ms": 150.0,
					"llm_failures":       0,
					"tool_failures":      0,
					"schema_violations":  0,
				},
				{
					"trace_key":          "exec:abc-2",
					"exec_id":            "abc-2",
					"events":             7,
					"last_kind":          "pipe.failed",
					"average_latency_ms": 0,
					"llm_failures":       2,
					"tool_failures":      0,
					"schema_violations":  1,
				},
			},
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"logs", "--url", srv.URL, "--last", "3"})
	if err != nil {
		t.Fatalf("Run(logs) error = %v", err)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}

	stdout := out.String()
	for _, want := range []string{
		"EXEC_ID",
		"LAST_KIND",
		"abc-1",
		"completed",
		"abc-2",
		"failed",
		"llm_fail=2",
		"schema_violations=1",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("logs output missing %q in:\n%s", want, stdout)
		}
	}
}

func TestRunLogsEmptyMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"traces": []map[string]any{},
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"logs", "--url", srv.URL}); err != nil {
		t.Fatalf("Run(logs) error = %v", err)
	}
	if !strings.Contains(out.String(), "no traces") {
		t.Fatalf("empty logs output = %q, want 'no traces'", out.String())
	}
}
