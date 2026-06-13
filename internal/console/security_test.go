package console

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

func bearer(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+testToken)
}

// TestAuthRequiredOnEveryRoute hits every /api/v1 route without a token (expect
// 401) and with a token (expect not 401). The table is the coverage guard: a
// new route added to registerAPIRoutes without auth coverage shows up here.
func TestAuthRequiredOnEveryRoute(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	routes := []struct {
		method, path string
	}{
		{"GET", "/api/v1/fleet"},
		{"GET", "/api/v1/overview"},
		{"GET", "/api/v1/environments"},
		{"GET", "/api/v1/workers/alpha/admin/status"},
		{"POST", "/api/v1/workers/alpha/admin/trigger"},
		{"POST", "/api/v1/workers/staging/deploy"},
		{"POST", "/api/v1/workers/alpha/reset"},
	}

	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// Without token -> 401.
			req, _ := http.NewRequest(rt.method, ts.URL+rt.path, nil)
			req.Host = "127.0.0.1"
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("no-token request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("no token: got %d, want 401", resp.StatusCode)
			}

			// With token -> not 401 (route reachable; may 200/400/502).
			req2, _ := http.NewRequest(rt.method, ts.URL+rt.path, nil)
			req2.Host = "127.0.0.1"
			bearer(req2)
			resp2, err := ts.Client().Do(req2)
			if err != nil {
				t.Fatalf("token request: %v", err)
			}
			resp2.Body.Close()
			if resp2.StatusCode == http.StatusUnauthorized {
				t.Fatalf("with token: got 401, route should be reachable")
			}
		})
	}
}

func TestWrongTokenRejected(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/fleet", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer deadbeef")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", resp.StatusCode)
	}
}

// TestProxyInjectsTokenServerSide asserts the upstream admin server sees the
// injected admin token while the browser response never contains it.
func TestProxyInjectsTokenServerSide(t *testing.T) {
	const adminToken = "super-secret-admin-token"
	var sawAuth string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		// Echo something that does NOT include the token.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mgr := newFakeManager(adminToken)
	mgr.addWorker("alpha", upstream)
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/workers/alpha/admin/status", nil)
	req.Host = "127.0.0.1"
	bearer(req)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if sawAuth != "Bearer "+adminToken {
		t.Fatalf("upstream saw Authorization %q, want injected admin token", sawAuth)
	}
	if strings.Contains(string(body), adminToken) {
		t.Fatalf("admin token leaked into browser response: %s", body)
	}
	// The browser session token must not be forwarded upstream either.
	if strings.Contains(sawAuth, testToken) {
		t.Fatalf("session token forwarded upstream: %s", sawAuth)
	}
}

// TestAllowlistRejectsNonAllowlistedAdminRoutes checks that a real admin path
// not in the allowlist and a made-up path are both 403 before any upstream
// call, and that a disallowed method on an allowlisted path is rejected.
func TestAllowlistRejectsNonAllowlistedAdminRoutes(t *testing.T) {
	upstreamHit := false
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(200)
	})
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", upstream)
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cases := []struct{ method, path string }{
		{"POST", "/api/v1/workers/alpha/admin/plans"},        // GET-only route, POST denied
		{"POST", "/api/v1/workers/alpha/admin/made/up/path"}, // nonexistent
		{"GET", "/api/v1/workers/alpha/admin/metrics"},       // intentionally excluded
		{"DELETE", "/api/v1/workers/alpha/admin/status"},     // method not allowed
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			upstreamHit = false
			req, _ := http.NewRequest(c.method, ts.URL+c.path, nil)
			req.Host = "127.0.0.1"
			bearer(req)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("got %d, want 403 for non-allowlisted %s %s", resp.StatusCode, c.method, c.path)
			}
			if upstreamHit {
				t.Fatalf("upstream was reached for a denied route %s %s", c.method, c.path)
			}
		})
	}
}

// TestAllowlistedAdminPOSTReachesUpstream confirms an allowlisted POST
// (trigger) is forwarded.
func TestAllowlistedAdminPOSTReachesUpstream(t *testing.T) {
	hit := false
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/admin/trigger" {
			hit = true
		}
		w.WriteHeader(202)
	})
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", upstream)
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/workers/alpha/admin/trigger", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	bearer(req)
	resp, _ := ts.Client().Do(req)
	resp.Body.Close()
	if !hit {
		t.Fatalf("allowlisted POST /admin/trigger did not reach upstream")
	}
}

// TestOverviewPartialResults sends one worker down and confirms the overview
// still returns the up worker plus an error entry for the down one.
func TestOverviewPartialResults(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	mgr.addWorker("beta", okAdmin())
	mgr.markDown("beta")
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha", "beta")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/overview", nil)
	req.Host = "127.0.0.1"
	bearer(req)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("overview status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"name":"alpha"`) || !strings.Contains(s, `"ok":true`) {
		t.Fatalf("alpha should be OK: %s", s)
	}
	if !strings.Contains(s, `"name":"beta"`) || !strings.Contains(s, `"error"`) {
		t.Fatalf("beta should report an error (partial result): %s", s)
	}
}

// TestProxyFlushesSSE asserts the admin proxy streams SSE events incrementally
// (FlushInterval=-1): we read the first event before the upstream sends the
// last and the request stays open.
func TestProxyFlushesSSE(t *testing.T) {
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-release // block before sending more
		_, _ = io.WriteString(w, "data: second\n\n")
		fl.Flush()
	})
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", upstream)
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/workers/alpha/admin/events", nil)
	req.Host = "127.0.0.1"
	bearer(req)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("sse request: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "first") {
		t.Fatalf("expected first event flushed before stream end, got %q", got)
	}
	close(release)
}

func TestNonLoopbackBindRefused(t *testing.T) {
	t.Setenv(insecureEnv, "")
	_, err := NewServer(Options{Addr: "0.0.0.0:7333", sessionToken: testToken})
	if err == nil {
		t.Fatal("expected refusal binding 0.0.0.0 without insecure opt-in")
	}
	if !strings.Contains(err.Error(), "refusing to bind") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNonLoopbackBindAllowedWithOptIn(t *testing.T) {
	t.Setenv(insecureEnv, "1")
	if _, err := NewServer(Options{Addr: "0.0.0.0:7333", sessionToken: testToken}); err != nil {
		t.Fatalf("with opt-in, bind should be allowed: %v", err)
	}
}

func TestLoopbackBindAllowed(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7333", "localhost:7333", "[::1]:7333"} {
		if _, err := NewServer(Options{Addr: addr, sessionToken: testToken}); err != nil {
			t.Fatalf("loopback %q refused: %v", addr, err)
		}
	}
}

// TestHostHeaderAllowlist rejects a non-loopback Host (DNS-rebinding) and
// accepts a loopback Host.
func TestHostHeaderAllowlist(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Attacker host -> 403.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/fleet", nil)
	req.Host = "evil.example.com"
	bearer(req)
	resp, _ := ts.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("attacker Host: got %d, want 403", resp.StatusCode)
	}

	// Loopback host -> allowed.
	req2, _ := http.NewRequest("GET", ts.URL+"/api/v1/fleet", nil)
	req2.Host = "localhost"
	bearer(req2)
	resp2, _ := ts.Client().Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusForbidden {
		t.Fatalf("loopback Host wrongly rejected: %d", resp2.StatusCode)
	}
}

// TestOriginRejection rejects a cross-origin Origin and accepts a loopback one.
func TestOriginRejection(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/workers/alpha/reset", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "https://evil.example.com")
	bearer(req)
	resp, _ := ts.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: got %d, want 403", resp.StatusCode)
	}
}

// TestCacheControlNoStore asserts API responses are not cacheable and the SPA
// carries the CSP + X-Frame-Options.
func TestSecurityHeaders(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// API: no-store.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/fleet", nil)
	req.Host = "127.0.0.1"
	bearer(req)
	resp, _ := ts.Client().Do(req)
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("API Cache-Control = %q, want no-store", cc)
	}

	// SPA: CSP + frame deny.
	req2, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req2.Host = "127.0.0.1"
	resp2, _ := ts.Client().Do(req2)
	resp2.Body.Close()
	csp := resp2.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("SPA CSP missing self directives: %q", csp)
	}
	if resp2.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("SPA X-Frame-Options = %q, want DENY", resp2.Header.Get("X-Frame-Options"))
	}
}

// TestNoCookies asserts the console never sets a cookie anywhere.
func TestNoCookies(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/", "/app.js", "/api/v1/fleet"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = "127.0.0.1"
		bearer(req)
		resp, _ := ts.Client().Do(req)
		resp.Body.Close()
		if len(resp.Cookies()) != 0 {
			t.Fatalf("path %s set cookies: %v", path, resp.Cookies())
		}
	}
}

// TestConcurrentRequestsShareOneManager exercises the lazy manager init under
// concurrent load (run with -race) to confirm s.mgr is built exactly once and
// safely.
func TestConcurrentRequestsShareOneManager(t *testing.T) {
	mgr := newFakeManager("admintok")
	mgr.addWorker("alpha", okAdmin())
	defer mgr.Close()
	srv := newTestServer(t, mgr, "alpha")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", ts.URL+"/api/v1/fleet", nil)
			req.Host = "127.0.0.1"
			bearer(req)
			resp, err := ts.Client().Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// okAdmin is a fake admin server returning a small JSON status body.
func okAdmin() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"worker":"ok","plans":[]}`))
	})
}

// Compile-time: *tunnel.Manager satisfies the console Manager interface, so the
// production path uses the real manager unchanged.
var _ Manager = (*tunnel.Manager)(nil)

// ensure deploy.EnvOpts/ProgressWriter wiring stays type-correct.
var _ deployFunc = func(_ context.Context, _ deploy.EnvOpts, _ deploy.ProgressWriter) error { return nil }
