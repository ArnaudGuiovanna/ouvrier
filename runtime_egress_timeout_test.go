package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// blackHoleServer returns a server whose handler never responds until the test
// releases it, plus the release func. Callers must release before Close() so the
// handler goroutine unblocks and Server.Close() does not hang.
func blackHoleServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-unblock:
		}
	}))
	release := func() { close(unblock) }
	return server, release
}

// TestPostWebhookTimesOutOnBlackHoledEndpoint proves the H5 fix: an endpoint
// that never responds must not pin the push forever. With a no-deadline context
// (as the stream/cron/durable callers pass) the egress deadline still bounds the
// request instead of relying on http.DefaultClient's absent timeout.
func TestPostWebhookTimesOutOnBlackHoledEndpoint(t *testing.T) {
	server, release := blackHoleServer(t)
	defer server.Close() // runs after release, so the handler has already returned
	defer release()

	restore := egressTimeout
	egressTimeout = 100 * time.Millisecond
	defer func() { egressTimeout = restore }()

	start := time.Now()
	err := postWebhook(context.Background(), server.URL, "payload")
	if err == nil {
		t.Fatal("postWebhook returned nil for a black-holed endpoint; want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("postWebhook blocked for %v; egress timeout not enforced", elapsed)
	}
}

// TestPostHTTPQueueTimesOutOnBlackHoledEndpoint covers the same guard for the
// HTTP queue push terminal, which shares egressContext.
func TestPostHTTPQueueTimesOutOnBlackHoledEndpoint(t *testing.T) {
	server, release := blackHoleServer(t)
	defer server.Close()
	defer release()

	restore := egressTimeout
	egressTimeout = 100 * time.Millisecond
	defer func() { egressTimeout = restore }()

	start := time.Now()
	err := publishQueue(context.Background(), server.URL, "payload")
	if err == nil {
		t.Fatal("publishQueue returned nil for a black-holed HTTP endpoint; want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("publishQueue blocked for %v; egress timeout not enforced", elapsed)
	}
}

// TestPostWebhookSucceedsWithinDeadline guards against the timeout wrapper
// breaking the normal fast path.
func TestPostWebhookSucceedsWithinDeadline(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := postWebhook(context.Background(), server.URL, `{"ok":true}`); err != nil {
		t.Fatalf("postWebhook returned error on a healthy endpoint: %v", err)
	}
	if gotBody != `{"ok":true}` {
		t.Fatalf("server received body %q, want the payload", gotBody)
	}
}
