package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// fakeFleetManager stands in for *tunnel.Manager: each worker name maps to a
// RoundTripper and a reported tunnel state. Unknown names or a nil transport
// surface a transport error, modeling a down host.
type fakeFleetManager struct {
	transports map[string]http.RoundTripper
	states     map[string]tunnel.State
}

func (m *fakeFleetManager) Transport(name string) (http.RoundTripper, error) {
	rt, ok := m.transports[name]
	if !ok || rt == nil {
		return nil, errors.New("worker " + name + ": tunnel is down")
	}
	return rt, nil
}

func (m *fakeFleetManager) States() map[string]tunnel.State { return m.states }
func (m *fakeFleetManager) Close() error                    { return nil }

// serverTransport mimics the real tunnel RoundTripper: the request URL's host
// is ignored (the real transport dials the tunnel socket), so we rewrite it to
// the backing httptest server and inject the bearer token in memory. This lets
// tests assert the CLI's adminapi.Client passes an empty token of its own.
type serverTransport struct {
	srv   *httptest.Server
	token string
}

func (t serverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	base := t.srv.URL // e.g. http://127.0.0.1:PORT
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(base, "http://")
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.srv.Client().Transport.RoundTrip(r)
}

// withFakeFleetManager swaps the fleet manager factory for one returning fm and
// restores the original when the test ends.
func withFakeFleetManager(t *testing.T, fm fleetManager) {
	t.Helper()
	orig := newFleetManager
	newFleetManager = func([]deploy.Deployment, tunnel.Options) (fleetManager, error) {
		return fm, nil
	}
	t.Cleanup(func() { newFleetManager = orig })
}

// fleetInventory writes a deployments.json with the given worker names and
// points OUVRIER_FLEET_PATH at it.
func fleetInventory(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/deployments.json"
	var deps []deploy.Deployment
	for _, n := range names {
		deps = append(deps, deploy.Deployment{Name: n, Host: n + ".example.com", AdminAddr: "127.0.0.1:9090"})
	}
	data, err := json.MarshalIndent(deploy.Inventory{Version: 1, Deployments: deps}, "", "  ")
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	t.Setenv("OUVRIER_FLEET_PATH", path)
}

// adminStatusServer returns an httptest server serving /admin/status (and an
// empty /admin/health) with the given status string, requiring the bearer
// token when want is non-empty.
func adminStatusServer(t *testing.T, statusValue, wantToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": statusValue, "sessions": 1})
		case "/admin/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cron_leases": []map[string]any{
					{"name": "cron:ab:0", "holder": "h", "fence": 1, "expires_at": "2026-06-12T10:00:30Z", "is_self": true},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStatusWorkerSingleTarget(t *testing.T) {
	const token = "secret-fleet-token"
	srv := adminStatusServer(t, "ok", token)
	fleetInventory(t, "alpha")
	withFakeFleetManager(t, &fakeFleetManager{
		transports: map[string]http.RoundTripper{"alpha": serverTransport{srv: srv, token: token}},
		states:     map[string]tunnel.State{"alpha": {Status: tunnel.StatusUp}},
	})

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"status", "--worker", "alpha"}); err != nil {
		t.Fatalf("status --worker error = %v", err)
	}

	got := out.String()
	for _, want := range []string{"=== alpha [up] ===", "alpha  status:            ok", "cron_leases:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, token) || strings.Contains(errOut.String(), token) {
		t.Fatalf("fleet output leaked token:\nstdout=%s\nstderr=%s", got, errOut.String())
	}
}

func TestStatusAllPartialFailure(t *testing.T) {
	srv := adminStatusServer(t, "ok", "")
	fleetInventory(t, "alpha", "bravo")
	withFakeFleetManager(t, &fakeFleetManager{
		transports: map[string]http.RoundTripper{
			"alpha": serverTransport{srv: srv},
			"bravo": nil, // down host
		},
		states: map[string]tunnel.State{
			"alpha": {Status: tunnel.StatusUp},
			"bravo": {Status: tunnel.StatusDegraded, LastError: "ssh exited"},
		},
	})

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"status", "--all"})
	if err == nil {
		t.Fatal("status --all with a down host returned nil, want partial-failure error")
	}
	if !errors.Is(err, errFleetPartial) {
		t.Fatalf("error = %v, want errFleetPartial", err)
	}

	got := out.String()
	// The healthy worker is still printed...
	if !strings.Contains(got, "=== alpha [up] ===") || !strings.Contains(got, "alpha  status:            ok") {
		t.Fatalf("healthy worker output missing in:\n%s", got)
	}
	// ...and the failed one is reported with its tunnel state.
	if !strings.Contains(got, "=== bravo [degraded] ===") || !strings.Contains(got, "error:") {
		t.Fatalf("failed worker not reported in:\n%s", got)
	}
}

func TestFleetTokenMaskingOnError(t *testing.T) {
	const token = "top-secret-token"
	fleetInventory(t, "alpha")
	// A transport whose error embeds the token, simulating a leak the CLI must
	// mask before printing.
	withFakeFleetManager(t, &fakeFleetManager{
		transports: map[string]http.RoundTripper{
			"alpha": roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed with token " + token)
			}),
		},
		states: map[string]tunnel.State{"alpha": {Status: tunnel.StatusDegraded}},
	})

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"status", "--worker", "alpha", "--token", token})
	if err == nil {
		t.Fatal("expected error from failing transport")
	}
	if strings.Contains(out.String(), token) || strings.Contains(errOut.String(), token) || strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked: stdout=%q stderr=%q err=%v", out.String(), errOut.String(), err)
	}
	if !strings.Contains(out.String(), "error:") {
		t.Fatalf("per-worker error not printed:\n%s", out.String())
	}
}

func TestFleetFlagMutualExclusion(t *testing.T) {
	cases := [][]string{
		{"status", "--worker", "a", "--all"},
		{"status", "--url", "http://x", "--worker", "a"},
		{"status", "--url", "http://x", "--all"},
		{"logs", "--worker", "a", "--all"},
		{"trace", "exec-1", "--worker", "a", "--all"},
		{"trace", "exec-1", "--url", "http://x", "--all"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(nil, &out, &errOut))
		err := app.Run(context.Background(), args)
		if !errors.Is(err, ErrUsage) {
			t.Fatalf("args %v: error = %v, want ErrUsage", args, err)
		}
	}
}

func TestLogsWorkerSingleTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/traces" {
			t.Errorf("path = %q, want /admin/traces", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"traces": []map[string]any{{"exec_id": "abc-1", "events": 3, "last_kind": "pipe.completed"}},
		})
	}))
	t.Cleanup(srv.Close)
	fleetInventory(t, "alpha")
	withFakeFleetManager(t, &fakeFleetManager{
		transports: map[string]http.RoundTripper{"alpha": serverTransport{srv: srv}},
		states:     map[string]tunnel.State{"alpha": {Status: tunnel.StatusUp}},
	})

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"logs", "--worker", "alpha"}); err != nil {
		t.Fatalf("logs --worker error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "alpha  EXEC_ID") || !strings.Contains(got, "alpha  abc-1") {
		t.Fatalf("logs fleet output missing prefixed rows in:\n%s", got)
	}
}

func TestTraceWorkerSingleTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/traces/exec-42" {
			t.Errorf("path = %q, want /admin/traces/exec-42", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"execution": map[string]any{"status": "completed"},
			"events":    []map[string]any{{"id": 1, "at": "t", "kind": "pipe.started"}},
		})
	}))
	t.Cleanup(srv.Close)
	fleetInventory(t, "alpha")
	withFakeFleetManager(t, &fakeFleetManager{
		transports: map[string]http.RoundTripper{"alpha": serverTransport{srv: srv}},
		states:     map[string]tunnel.State{"alpha": {Status: tunnel.StatusUp}},
	})

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"trace", "exec-42", "--worker", "alpha"}); err != nil {
		t.Fatalf("trace --worker error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "alpha  exec_id:           exec-42") || !strings.Contains(got, "alpha  status:            completed") {
		t.Fatalf("trace fleet output missing prefixed detail in:\n%s", got)
	}
}

func TestUnknownWorkerIsUsageError(t *testing.T) {
	fleetInventory(t, "alpha")
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"status", "--worker", "ghost"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("unknown worker error = %v, want ErrUsage", err)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
