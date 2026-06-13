package console

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// fakeManager is a console.Manager that reaches an httptest admin server per
// worker through an in-memory transport, injecting a fixed admin token the way
// the real tunnel manager does. No ssh is spawned. A worker can be marked down
// to exercise partial-result / unreachable paths.
type fakeManager struct {
	mu        sync.Mutex
	upstreams map[string]*httptest.Server // name -> fake admin server
	down      map[string]bool             // name -> tunnel down
	states    map[string]tunnel.State
	// injectedToken is the admin token the transport injects upstream; the test
	// asserts the upstream saw it and the browser never did.
	injectedToken string
	resetCalls    []string
}

func newFakeManager(token string) *fakeManager {
	return &fakeManager{
		upstreams:     map[string]*httptest.Server{},
		down:          map[string]bool{},
		states:        map[string]tunnel.State{},
		injectedToken: token,
	}
}

func (m *fakeManager) addWorker(name string, h http.Handler) {
	srv := httptest.NewServer(h)
	m.mu.Lock()
	m.upstreams[name] = srv
	m.states[name] = tunnel.State{Status: tunnel.StatusUp}
	m.mu.Unlock()
}

func (m *fakeManager) markDown(name string) {
	m.mu.Lock()
	m.down[name] = true
	m.states[name] = tunnel.State{Status: tunnel.StatusDown, LastError: "fake: tunnel down"}
	m.mu.Unlock()
}

func (m *fakeManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.upstreams {
		s.Close()
	}
}

// fakeRoundTripper rewrites the request to the worker's httptest upstream and
// injects the admin token, mimicking tunnel.Manager.Transport.
type fakeRoundTripper struct {
	m    *fakeManager
	name string
}

func (rt *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.m.mu.Lock()
	down := rt.m.down[rt.name]
	srv := rt.m.upstreams[rt.name]
	token := rt.m.injectedToken
	rt.m.mu.Unlock()
	if down || srv == nil {
		return nil, &net.OpError{Op: "dial", Err: errFakeDown}
	}
	target, _ := net.ResolveTCPAddr("tcp", srv.Listener.Addr().String())
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	out.URL.Host = target.String()
	out.Host = target.String()
	if token != "" {
		out.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultTransport.RoundTrip(out)
}

var errFakeDown = errFake("tunnel down")

type errFake string

func (e errFake) Error() string { return string(e) }
func (e errFake) Timeout() bool { return false }

func (m *fakeManager) Transport(name string) (http.RoundTripper, error) {
	m.mu.Lock()
	_, ok := m.upstreams[name]
	m.mu.Unlock()
	if !ok {
		return nil, errFake("unknown worker " + name)
	}
	return &fakeRoundTripper{m: m, name: name}, nil
}

func (m *fakeManager) Dial(ctx context.Context, name string) (net.Conn, error) {
	m.mu.Lock()
	down := m.down[name]
	srv := m.upstreams[name]
	m.mu.Unlock()
	if down || srv == nil {
		return nil, errFake("dial " + name + ": tunnel down")
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
}

func (m *fakeManager) States() map[string]tunnel.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]tunnel.State, len(m.states))
	for k, v := range m.states {
		out[k] = v
	}
	return out
}

func (m *fakeManager) Reset(name string) error {
	m.mu.Lock()
	m.resetCalls = append(m.resetCalls, name)
	m.mu.Unlock()
	return nil
}

func (m *fakeManager) Close() error { m.closeAll(); return nil }

// writeFleet creates a temp project dir with a deployments.json inventory
// listing the given worker names (host = 127.0.0.1) and a minimal pip.yaml.
func writeFleet(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	inv := deploy.Inventory{Version: 1}
	for _, n := range names {
		inv.Deployments = append(inv.Deployments, deploy.Deployment{
			Name: n, Host: "127.0.0.1", Service: "ouvrier-" + n,
		})
	}
	b, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployments.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	pip := "name: testproj\nversion: 0.1.0\ndeploy:\n  staging:\n    hosts: [user@127.0.0.1]\n"
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte(pip), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newTestServer builds a console Server wired to a fake manager and a fixed
// session token, with the inventory taken from names.
func newTestServer(t *testing.T, mgr *fakeManager, names ...string) *Server {
	t.Helper()
	dir := writeFleet(t, names...)
	srv, err := NewServer(Options{
		Addr:         "127.0.0.1:0",
		Dir:          dir,
		FleetPath:    dir + "/deployments.json",
		sessionToken: testToken,
		newManager: func(_ []deploy.Deployment, _ tunnel.Options) (Manager, error) {
			return mgr, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

const testToken = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
