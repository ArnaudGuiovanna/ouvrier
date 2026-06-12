package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

// newSplitTestHandlers builds the OUVRIER_ADMIN_ADDR handler pair for a
// worker with one public trigger (GET /health, direct reply) that needs no
// LLM provider.
func newSplitTestHandlers(t *testing.T, rt httpRuntime) (publicHandler, adminHandler http.Handler) {
	t.Helper()
	if rt.stateStore == nil {
		rt.stateStore = state.NewMemoryStore()
	}
	if rt.eventStream == nil {
		stream, err := events.NewEventStream()
		if err != nil {
			t.Fatalf("NewEventStream returned error: %v", err)
		}
		rt.eventStream = stream
	}
	publicHandler, adminHandler, err := newSplitHTTPCompatibleHandlersWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newSplitHTTPCompatibleHandlersWithRuntime returned error: %v", err)
	}
	return publicHandler, adminHandler
}

func splitTestStatus(handler http.Handler, method, target, bearer string) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestSplitHandlersMoveAdminSurfaceOffPublicHandler(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_METRICS_PUBLIC", "")
	publicHandler, adminHandler := newSplitTestHandlers(t, httpRuntime{})

	// The public handler keeps the trigger and loses the whole admin surface.
	if got := splitTestStatus(publicHandler, http.MethodGet, "/health", ""); got != http.StatusOK {
		t.Fatalf("public GET /health = %d, want %d", got, http.StatusOK)
	}
	for _, probe := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/admin/health"},
		{http.MethodGet, "/admin/status"},
		{http.MethodGet, "/admin/events"},
		{http.MethodPost, "/admin/trigger"},
		{http.MethodGet, "/admin/approvals"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/dev"},
	} {
		if got := splitTestStatus(publicHandler, probe.method, probe.target, ""); got != http.StatusNotFound {
			t.Fatalf("public %s %s = %d, want %d (admin surface must leave the public mux)", probe.method, probe.target, got, http.StatusNotFound)
		}
	}

	// The admin handler carries admin routes plus /metrics and nothing else.
	if got := splitTestStatus(adminHandler, http.MethodGet, "/admin/health", ""); got != http.StatusOK {
		t.Fatalf("admin GET /admin/health = %d, want %d", got, http.StatusOK)
	}
	if got := splitTestStatus(adminHandler, http.MethodGet, "/metrics", ""); got != http.StatusOK {
		t.Fatalf("admin GET /metrics = %d, want %d", got, http.StatusOK)
	}
	if got := splitTestStatus(adminHandler, http.MethodGet, "/health", ""); got != http.StatusNotFound {
		t.Fatalf("admin GET /health (public trigger) = %d, want %d", got, http.StatusNotFound)
	}
}

func TestSplitHandlersKeepMetricsOnPublicWithOptIn(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_METRICS_PUBLIC", "1")
	publicHandler, adminHandler := newSplitTestHandlers(t, httpRuntime{})

	if got := splitTestStatus(publicHandler, http.MethodGet, "/metrics", ""); got != http.StatusOK {
		t.Fatalf("public GET /metrics with OUVRIER_METRICS_PUBLIC=1 = %d, want %d", got, http.StatusOK)
	}
	if got := splitTestStatus(adminHandler, http.MethodGet, "/metrics", ""); got != http.StatusOK {
		t.Fatalf("admin GET /metrics with OUVRIER_METRICS_PUBLIC=1 = %d, want %d", got, http.StatusOK)
	}
	// The rest of the admin surface stays off the public handler.
	if got := splitTestStatus(publicHandler, http.MethodGet, "/admin/health", ""); got != http.StatusNotFound {
		t.Fatalf("public GET /admin/health = %d, want %d", got, http.StatusNotFound)
	}
}

func TestSplitHandlersEnforceAdminTokenOnAdminHandler(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "")
	t.Setenv("OUVRIER_METRICS_PUBLIC", "1")
	publicHandler, adminHandler := newSplitTestHandlers(t, httpRuntime{adminToken: "secret-admin-token"})

	// Bearer enforcement on the dedicated listener is identical to v0.2.
	if got := splitTestStatus(adminHandler, http.MethodGet, "/admin/health", ""); got != http.StatusUnauthorized {
		t.Fatalf("admin GET /admin/health without bearer = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := splitTestStatus(adminHandler, http.MethodGet, "/admin/health", "wrong-token"); got != http.StatusForbidden {
		t.Fatalf("admin GET /admin/health with wrong bearer = %d, want %d", got, http.StatusForbidden)
	}
	if got := splitTestStatus(adminHandler, http.MethodGet, "/admin/health", "secret-admin-token"); got != http.StatusOK {
		t.Fatalf("admin GET /admin/health with bearer = %d, want %d", got, http.StatusOK)
	}
	if got := splitTestStatus(adminHandler, http.MethodGet, "/metrics", ""); got != http.StatusUnauthorized {
		t.Fatalf("admin GET /metrics without bearer = %d, want %d", got, http.StatusUnauthorized)
	}
	// A publicly re-exposed /metrics keeps the same bearer auth.
	if got := splitTestStatus(publicHandler, http.MethodGet, "/metrics", ""); got != http.StatusUnauthorized {
		t.Fatalf("public GET /metrics without bearer = %d, want %d", got, http.StatusUnauthorized)
	}
	if got := splitTestStatus(publicHandler, http.MethodGet, "/metrics", "secret-admin-token"); got != http.StatusOK {
		t.Fatalf("public GET /metrics with bearer = %d, want %d", got, http.StatusOK)
	}
	// The public trigger never required the admin token.
	if got := splitTestStatus(publicHandler, http.MethodGet, "/health", ""); got != http.StatusOK {
		t.Fatalf("public GET /health = %d, want %d", got, http.StatusOK)
	}
}

func splitTestGet(t *testing.T, addr, path string) int {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s%s returned error: %v", addr, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestServeSplitHTTPServesPublicAndAdminListeners(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_METRICS_PUBLIC", "")
	publicHandler, adminHandler := newSplitTestHandlers(t, httpRuntime{})

	publicAddr := localRuntimeAddr(t)
	adminAddr := localRuntimeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveSplitHTTPWithContext(ctx, publicAddr, publicHandler, adminAddr, adminHandler)
	}()
	waitAdminHealth(t, adminAddr)

	// /admin/* and /metrics answer only on the admin port.
	if got := splitTestGet(t, adminAddr, "/metrics"); got != http.StatusOK {
		t.Fatalf("admin listener GET /metrics = %d, want %d", got, http.StatusOK)
	}
	if got := splitTestGet(t, publicAddr, "/admin/health"); got != http.StatusNotFound {
		t.Fatalf("public listener GET /admin/health = %d, want %d", got, http.StatusNotFound)
	}
	if got := splitTestGet(t, publicAddr, "/metrics"); got != http.StatusNotFound {
		t.Fatalf("public listener GET /metrics = %d, want %d", got, http.StatusNotFound)
	}
	// Triggers are unaffected: served on the public port, absent on admin.
	if got := splitTestGet(t, publicAddr, "/health"); got != http.StatusOK {
		t.Fatalf("public listener GET /health = %d, want %d", got, http.StatusOK)
	}
	if got := splitTestGet(t, adminAddr, "/health"); got != http.StatusNotFound {
		t.Fatalf("admin listener GET /health = %d, want %d", got, http.StatusNotFound)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serveSplitHTTPWithContext returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveSplitHTTPWithContext did not stop after cancellation")
	}
}

func TestServeAdminOnlyHTTPSplitsAdminListener(t *testing.T) {
	// Cron and stream workers expose only the admin surface over HTTP; with
	// OUVRIER_ADMIN_ADDR set it moves to the dedicated listener and the
	// public addr answers 404 for everything.
	t.Setenv("OUVRIER_ENV", "dev")
	publicAddr := localRuntimeAddr(t)
	adminAddr := localRuntimeAddr(t)
	t.Setenv("OUVRIER_ADMIN_ADDR", adminAddr)

	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newAdminHandlerWithRuntime(nil, httpRuntime{stateStore: state.NewMemoryStore(), eventStream: stream})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveAdminOnlyHTTPWithContext(ctx, publicAddr, handler)
	}()
	waitAdminHealth(t, adminAddr)

	if got := splitTestGet(t, publicAddr, "/admin/health"); got != http.StatusNotFound {
		t.Fatalf("public listener GET /admin/health = %d, want %d", got, http.StatusNotFound)
	}
	if got := splitTestGet(t, publicAddr, "/metrics"); got != http.StatusNotFound {
		t.Fatalf("public listener GET /metrics = %d, want %d", got, http.StatusNotFound)
	}
	if got := splitTestGet(t, adminAddr, "/metrics"); got != http.StatusOK {
		t.Fatalf("admin listener GET /metrics = %d, want %d", got, http.StatusOK)
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serveAdminOnlyHTTPWithContext returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveAdminOnlyHTTPWithContext did not stop after cancellation")
	}
}

func TestHandlerKeepsCombinedSurfaceWhenAdminAddrSet(t *testing.T) {
	// The Handler() test seam is an in-process surface: it always returns the
	// combined v0.2 handler — trigger routes, /admin/*, and /metrics together —
	// even when OUVRIER_ADMIN_ADDR would make Run split listeners.
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_ADMIN_ADDR", "127.0.0.1:9090")
	handler, err := Handler(
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	)
	if err != nil {
		t.Fatalf("Handler returned error: %v", err)
	}
	for _, target := range []string{"/health", "/admin/health", "/metrics"} {
		if got := splitTestStatus(handler, http.MethodGet, target, ""); got != http.StatusOK {
			t.Fatalf("Handler GET %s = %d, want %d (combined seam regardless of OUVRIER_ADMIN_ADDR)", target, got, http.StatusOK)
		}
	}
}

func TestRunRefusesNonLoopbackAdminAddr(t *testing.T) {
	t.Setenv("OUVRIER_STATE_BACKEND", "memory")
	t.Setenv("OUVRIER_ADMIN_ADDR", "0.0.0.0:9090")
	t.Setenv("OUVRIER_ADMIN_INSECURE", "")

	err := Run(localRuntimeAddr(t),
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want refusal for non-loopback OUVRIER_ADMIN_ADDR")
	}
	if !strings.Contains(err.Error(), "OUVRIER_ADMIN_ADDR") {
		t.Fatalf("Run error = %v, want it to name OUVRIER_ADMIN_ADDR", err)
	}
}
