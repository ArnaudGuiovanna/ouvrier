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
)

func TestRunTracePrintsTimeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const want = "/admin/traces/exec-42"
		if r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"execution": map[string]any{
				"exec_id":      "exec-42",
				"status":       "completed",
				"started_at":   "2026-05-24T10:00:00Z",
				"completed_at": "2026-05-24T10:00:01Z",
			},
			"sessions":          1,
			"schema_violations": 0,
			"last_event_id":     5,
			"events": []map[string]any{
				{
					"id":      1,
					"at":      "2026-05-24T10:00:00Z",
					"kind":    "pipe.started",
					"payload": map[string]any{"pipe": "triage"},
				},
				{
					"id":      2,
					"at":      "2026-05-24T10:00:00Z",
					"kind":    "llm.call.completed",
					"payload": map[string]any{"input_tokens": 12, "output_tokens": 34},
				},
				{
					"id":   5,
					"at":   "2026-05-24T10:00:01Z",
					"kind": "pipe.completed",
				},
			},
		})
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"trace", "exec-42", "--url", srv.URL})
	if err != nil {
		t.Fatalf("Run(trace) error = %v", err)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}

	stdout := out.String()
	for _, want := range []string{
		"exec_id:           exec-42",
		"status:            completed",
		"sessions:          1",
		"events:            3",
		"pipe.started",
		"llm.call.completed",
		"pipe.completed",
		"input_tokens=12",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("trace output missing %q in:\n%s", want, stdout)
		}
	}
}

func TestRunTraceRequiresExecID(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"trace"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run(trace) error = %v, want ErrUsage", err)
	}
}

func TestRunTracePropagates404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":"not_found"}`))
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"trace", "missing", "--url", srv.URL})
	if err == nil {
		t.Fatal("Run(trace) error = nil, want HTTP error")
	}
	var httpErr adminHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want adminHTTPError", err)
	}
	if httpErr.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", httpErr.Status)
	}
}
