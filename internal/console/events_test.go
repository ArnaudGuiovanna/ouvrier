package console

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonlEventStream is a fake /admin/events?format=jsonl&follow=true upstream:
// it writes the given JSONL lines (flushing each) then ends.
func jsonlEventStream(lines []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "jsonl" {
			http.Error(w, "want format=jsonl", 400)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n")
			fl.Flush()
		}
	})
}

// TestEventsFanInTagsWorkerAndSynthesizesUnreachable runs two workers — one
// streaming JSONL events, one down — and asserts the SSE fan-in tags each event
// with its worker name and emits a synthetic console.worker_unreachable for the
// down target.
func TestEventsFanInTagsWorkerAndSynthesizesUnreachable(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", jsonlEventStream([]string{
		`{"id":1,"kind":"run.started"}`,
		`{"id":2,"kind":"run.finished"}`,
	}))
	mgr.addWorker("beta", okAdmin())
	mgr.markDown("beta")
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha", "beta")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	var sawAlpha, sawUnreachable bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if strings.Contains(payload, `"worker":"alpha"`) && strings.Contains(payload, "run.started") {
			sawAlpha = true
		}
		if strings.Contains(payload, `"worker":"beta"`) && strings.Contains(payload, "console.worker_unreachable") {
			sawUnreachable = true
		}
	}
	if !sawAlpha {
		t.Error("did not see tagged alpha event")
	}
	if !sawUnreachable {
		t.Error("did not see synthetic console.worker_unreachable for down beta")
	}
}

// TestEventsAuthViaQueryParam confirms the SSE stream accepts the token as the
// access_token query param (EventSource cannot set headers) but still rejects a
// wrong token there, and that the query param does NOT authorize other routes.
func TestEventsAuthViaQueryParam(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", jsonlEventStream([]string{`{"id":1,"kind":"x"}`}))
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Correct token via query param -> 200.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events?access_token="+testToken, nil)
	req.Host = "127.0.0.1"
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("events with query token: got %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong token via query param -> 401.
	req2, _ := http.NewRequest("GET", ts.URL+"/api/v1/events?access_token=nope", nil)
	req2.Host = "127.0.0.1"
	resp2, _ := ts.Client().Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("events with bad query token: got %d, want 401", resp2.StatusCode)
	}

	// Query param must NOT authorize a different route (e.g. fleet).
	req3, _ := http.NewRequest("GET", ts.URL+"/api/v1/fleet?access_token="+testToken, nil)
	req3.Host = "127.0.0.1"
	resp3, _ := ts.Client().Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Fatalf("fleet with query token: got %d, want 401 (query token is events-only)", resp3.StatusCode)
	}

	// Query param must NOT authorize a mutating route. The method guard makes
	// this structurally impossible; pin it so a refactor can't regress it.
	for _, path := range []string{"/api/v1/workers/w1/deploy", "/api/v1/workers/w1/reset"} {
		req4, _ := http.NewRequest("POST", ts.URL+path+"?access_token="+testToken, nil)
		req4.Host = "127.0.0.1"
		req4.Header.Set("Origin", "http://127.0.0.1")
		resp4, err := ts.Client().Do(req4)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp4.Body.Close()
		if resp4.StatusCode != 401 {
			t.Fatalf("POST %s with query token: got %d, want 401 (query token is events-only)", path, resp4.StatusCode)
		}
	}
}
