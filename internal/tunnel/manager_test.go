package tunnel

// Lifecycle tests drive the Manager through the tunnelRunner fake: states,
// backoff, refcounted idle close, socket hygiene, and lazy independence of
// workers. All timing is injected (fake sleep, manually triggered idle
// timers, channel-based state observation) so the tests are deterministic
// under -race.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// --- fakes -----------------------------------------------------------------

// fakeProc is a controllable stand-in for one ssh -N process. The fake
// runner backs it with a real local listener so "socket connectable" state
// detection runs the production code path.
type fakeProc struct {
	srv    *http.Server
	exitCh chan error
	once   sync.Once

	mu     sync.Mutex
	stderr string
}

func (p *fakeProc) Wait() error { return <-p.exitCh }

func (p *fakeProc) Kill() error {
	p.terminate(errors.New("signal: killed"))
	return nil
}

// die simulates the ssh process exiting on its own with the given stderr.
func (p *fakeProc) die(stderr string, err error) {
	p.mu.Lock()
	p.stderr = stderr
	p.mu.Unlock()
	p.terminate(err)
}

func (p *fakeProc) terminate(err error) {
	p.once.Do(func() {
		if p.srv != nil {
			_ = p.srv.Close()
		}
		p.exitCh <- err
	})
}

func (p *fakeProc) Stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr
}

// fakeRunner records every Start and serves handler on a real listener bound
// at the requested local address, exactly where ssh would bind its forward.
type fakeRunner struct {
	handler http.Handler

	mu         sync.Mutex
	starts     []Forward
	opts       []deploy.ConnectOpts
	procs      []*fakeProc
	failByHost map[string]int // remaining Start failures per host; -1 = always
}

func (f *fakeRunner) Start(opts deploy.ConnectOpts, fwd Forward) (process, error) {
	f.mu.Lock()
	f.starts = append(f.starts, fwd)
	f.opts = append(f.opts, opts)
	remaining := f.failByHost[opts.Host]
	if remaining != 0 {
		if remaining > 0 {
			f.failByHost[opts.Host] = remaining - 1
		}
		f.mu.Unlock()
		return nil, fmt.Errorf("%w: spawn ssh: connection refused (fake, host %s)", ErrTunnel, opts.Host)
	}
	handler := f.handler
	f.mu.Unlock()

	ln, err := net.Listen(fwd.Network, fwd.LocalAddr)
	if err != nil {
		return nil, fmt.Errorf("%w: fake bind %s: %w", ErrTunnel, fwd.LocalAddr, err)
	}
	p := &fakeProc{exitCh: make(chan error, 1)}
	p.srv = &http.Server{Handler: handler}
	go func() { _ = p.srv.Serve(ln) }()

	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	return p, nil
}

func (f *fakeRunner) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func (f *fakeRunner) lastProc(t *testing.T) *fakeProc {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.procs) == 0 {
		t.Fatal("fake runner spawned no process")
	}
	return f.procs[len(f.procs)-1]
}

// fakeRemote is the deploy.RemoteRunner fake behind the remote token fetch.
type fakeRemote struct {
	mu     sync.Mutex
	envFor func(call int) string // .env content for the call-th SSH (1-based)
	errFor func(call int) error  // optional per-call error (1-based); nil = use err
	err    error
	cmds   []string
}

func (f *fakeRemote) SSH(_ context.Context, _ deploy.ConnectOpts, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, command)
	if f.errFor != nil {
		if err := f.errFor(len(f.cmds)); err != nil {
			return "", err
		}
	} else if f.err != nil {
		return "", f.err
	}
	return f.envFor(len(f.cmds)), nil
}

func (f *fakeRemote) SSHIn(context.Context, deploy.ConnectOpts, string, []byte) (string, error) {
	return "", errors.New("fakeRemote: SSHIn not expected")
}
func (f *fakeRemote) SCP(context.Context, deploy.ConnectOpts, string, string) error {
	return errors.New("fakeRemote: SCP not expected")
}
func (f *fakeRemote) SCPData(context.Context, deploy.ConnectOpts, []byte, string) error {
	return errors.New("fakeRemote: SCPData not expected")
}

func (f *fakeRemote) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmds)
}

// --- harness ---------------------------------------------------------------

type stateEvent struct {
	name string
	st   State
}

type fakeTimer struct{}

func (fakeTimer) Stop() bool { return true }

// testHarness wires a Manager to fully injected timings: backoff sleeps park
// until stop (no hot retry loops) unless a test swaps in its own sleep, and
// idle timers fire only when the test invokes them.
type testHarness struct {
	cfg     managerConfig
	events  chan stateEvent
	idleArm chan func() // one func per armed idle timer; call it to fire
}

func newHarness(r tunnelRunner) *testHarness {
	h := &testHarness{
		events:  make(chan stateEvent, 256),
		idleArm: make(chan func(), 16),
	}
	cfg := defaultManagerConfig()
	cfg.runner = r
	cfg.dialInterval = time.Millisecond
	cfg.connectTimeout = 5 * time.Second
	cfg.jitter = func(d time.Duration) time.Duration { return d }
	cfg.sleep = func(stop <-chan struct{}, _ time.Duration) bool {
		<-stop
		return false
	}
	cfg.afterFunc = func(_ time.Duration, f func()) idleTimer {
		h.idleArm <- f
		return fakeTimer{}
	}
	cfg.onState = func(name string, st State) {
		h.events <- stateEvent{name: name, st: st}
	}
	h.cfg = cfg
	return h
}

func (h *testHarness) waitState(t *testing.T, name string, status Status) State {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.events:
			if ev.name == name && ev.st.Status == status {
				return ev.st
			}
		case <-deadline:
			t.Fatalf("timed out waiting for worker %s to reach state %s", name, status)
		}
	}
}

// fireIdle waits for an idle timer to be armed and fires it.
func (h *testHarness) fireIdle(t *testing.T) {
	t.Helper()
	select {
	case f := <-h.idleArm:
		f()
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an idle timer to be armed")
	}
}

func (h *testHarness) idleArmed() bool {
	select {
	case f := <-h.idleArm:
		h.idleArm <- f
		return true
	default:
		return false
	}
}

// pinHosts writes a project ouvrier.known_hosts pinning the given hosts, so
// RequirePinnedHost passes without touching a network.
func pinHosts(t *testing.T, dir string, hosts ...string) {
	t.Helper()
	var b strings.Builder
	for _, h := range hosts {
		fmt.Fprintf(&b, "%s ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeFakeFakeFakeFakeFakeFakeFakeFakeFakeFake\n", h)
	}
	if err := os.WriteFile(filepath.Join(dir, deploy.KnownHostsFile), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testDeployment(name, host string) deploy.Deployment {
	return deploy.Deployment{
		Name:      name,
		Host:      host,
		User:      "deploy",
		Path:      "/srv/" + name,
		AdminAddr: "127.0.0.1:9090",
	}
}

// newTestManager builds a Manager over the fake runner with one pinned
// worker w1@h1 and registers cleanup.
func newTestManager(t *testing.T, h *testHarness, opts Options, deployments ...deploy.Deployment) *Manager {
	t.Helper()
	if len(deployments) == 0 {
		deployments = []deploy.Deployment{testDeployment("w1", "h1")}
	}
	if opts.Dir == "" {
		dir := t.TempDir()
		hosts := make([]string, 0, len(deployments))
		for _, d := range deployments {
			hosts = append(hosts, deploy.KnownHostsHostname(d.Host, d.Port))
		}
		pinHosts(t, dir, hosts...)
		opts.Dir = dir
	}
	if opts.SocketDir == "" {
		opts.SocketDir = filepath.Join(t.TempDir(), "tun")
	}
	if opts.Remote == nil {
		opts.Remote = &fakeRemote{envFor: func(int) string { return "" }}
	}
	m, err := newManager(deployments, opts, h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
}

func get(t *testing.T, rt http.RoundTripper, url string) (*http.Response, string, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp, string(body), nil
}

// --- lifecycle -------------------------------------------------------------

func TestLazyStartLifecycleStates(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})

	// Lazy: nothing spawns at construction.
	if got := fr.startCount(); got != 0 {
		t.Fatalf("runner started %d processes before first use; want 0", got)
	}
	if st := m.States()["w1"]; st.Status != StatusDown {
		t.Fatalf("initial state = %s, want %s", st.Status, StatusDown)
	}

	rt, err := m.Transport("w1")
	if err != nil {
		t.Fatal(err)
	}
	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("round trip = %d %q, want 200 ok", resp.StatusCode, body)
	}

	// Explicit state machine: connecting then up.
	h.waitState(t, "w1", StatusConnecting)
	h.waitState(t, "w1", StatusUp)
	if st := m.States()["w1"]; st.Status != StatusUp {
		t.Fatalf("state after round trip = %s, want %s", st.Status, StatusUp)
	}
	if got := fr.startCount(); got != 1 {
		t.Fatalf("runner started %d processes, want 1", got)
	}

	// The forward targets the unix socket in the manager's socket dir and
	// the worker's admin addr; the connect opts carry the pinned known_hosts.
	fr.mu.Lock()
	fwd, opts := fr.starts[0], fr.opts[0]
	fr.mu.Unlock()
	wantSock := filepath.Join(m.sockDir, "w1.sock")
	if fwd.Network != "unix" || fwd.LocalAddr != wantSock {
		t.Fatalf("forward = %+v, want unix socket %s", fwd, wantSock)
	}
	if fwd.RemoteAddr != "127.0.0.1:9090" {
		t.Fatalf("forward remote = %s, want 127.0.0.1:9090", fwd.RemoteAddr)
	}
	if opts.Host != "h1" || opts.User != "deploy" || !strings.HasSuffix(opts.KnownHosts, deploy.KnownHostsFile) {
		t.Fatalf("connect opts = %+v, want pinned h1", opts)
	}

	// Socket dir is 0700.
	fi, err := os.Stat(m.sockDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket dir mode = %o, want 700", perm)
	}
}

func TestProcessDeathDegradesWithStderr(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})
	rt, _ := m.Transport("w1")
	if _, _, err := get(t, rt, "http://w1/admin/health"); err != nil {
		t.Fatal(err)
	}
	h.waitState(t, "w1", StatusUp)

	fr.lastProc(t).die("kex_exchange_identification: read: Connection reset by peer", errors.New("exit status 255"))
	st := h.waitState(t, "w1", StatusDegraded)
	if !strings.Contains(st.LastError, "Connection reset by peer") {
		t.Fatalf("degraded state must surface ssh stderr verbatim, got %q", st.LastError)
	}
	if !strings.Contains(st.LastError, "exit status 255") {
		t.Fatalf("degraded state must keep the exit error, got %q", st.LastError)
	}

	// While the backoff retry is pending, requests fail fast with the last
	// error instead of hanging.
	_, _, err := get(t, rt, "http://w1/admin/health")
	if err == nil || !strings.Contains(err.Error(), "Connection reset by peer") {
		t.Fatalf("request during backoff = %v, want fail-fast with last error", err)
	}
	if !errors.Is(err, ErrTunnel) {
		t.Fatalf("error must wrap ErrTunnel, got %v", err)
	}
}

func TestBackoffScheduleDoublesWithCap(t *testing.T) {
	fr := &fakeRunner{handler: okHandler(), failByHost: map[string]int{"h1": 7}}
	h := newHarness(fr)
	var sleepMu sync.Mutex
	var sleeps []time.Duration
	h.cfg.sleep = func(stop <-chan struct{}, d time.Duration) bool {
		sleepMu.Lock()
		sleeps = append(sleeps, d)
		sleepMu.Unlock()
		select {
		case <-stop:
			return false
		default:
			return true
		}
	}
	m := newTestManager(t, h, Options{Token: "tok-explicit"})

	rt, _ := m.Transport("w1")
	// The first request triggers the lazy open. Depending on how fast the
	// supervisor races through its (instant fake) backoffs, the waiter sees
	// either an early failed attempt or the eventual success — both are
	// valid; the backoff schedule below is what this test pins down.
	_, _, _ = get(t, rt, "http://w1/admin/health")
	// The supervisor keeps retrying through backoff; the 8th attempt binds.
	h.waitState(t, "w1", StatusUp)
	if resp, _, err := get(t, rt, "http://w1/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request after recovery = %v/%v, want 200", resp, err)
	}

	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	sleepMu.Lock()
	got := append([]time.Duration(nil), sleeps...)
	sleepMu.Unlock()
	if len(got) < len(want) {
		t.Fatalf("recorded %d backoff sleeps %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("backoff sleep %d = %s, want %s (all: %v)", i, got[i], w, got)
		}
	}
}

func TestIdleCloseAndLazyReopen(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})
	rt, _ := m.Transport("w1")
	if _, _, err := get(t, rt, "http://w1/admin/health"); err != nil {
		t.Fatal(err)
	}
	h.waitState(t, "w1", StatusUp)
	sock := filepath.Join(m.sockDir, "w1.sock")
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket missing while up: %v", err)
	}

	// Zero in-flight requests armed the idle timer; firing it closes the
	// tunnel: process killed, socket unlinked, state down.
	h.fireIdle(t)
	st := h.waitState(t, "w1", StatusDown)
	if !strings.Contains(st.LastError, "idle") {
		t.Fatalf("down state should record the idle reason, got %q", st.LastError)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be unlinked after idle close, stat err = %v", err)
	}

	// Lazily reopenable: the next request spawns a fresh process.
	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("round trip after reopen = %v %v %q", err, resp, body)
	}
	if got := fr.startCount(); got != 2 {
		t.Fatalf("runner started %d processes, want 2 (one per open)", got)
	}
}

func TestInFlightRefcountBlocksIdleClose(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})
	rt, _ := m.Transport("w1")

	req1, _ := http.NewRequest(http.MethodGet, "http://w1/admin/health", nil)
	resp1, err := rt.RoundTrip(req1)
	if err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodGet, "http://w1/admin/health", nil)
	resp2, err := rt.RoundTrip(req2)
	if err != nil {
		t.Fatal(err)
	}

	// Two in flight; finishing one must not arm the idle timer.
	_, _ = io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if h.idleArmed() {
		t.Fatal("idle timer armed while a request is still in flight")
	}
	// Closing the last body drops the refcount to zero and arms it.
	_, _ = io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	h.fireIdle(t)
	h.waitState(t, "w1", StatusDown)
}

func TestDialRefcountsLikeTransport(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})

	conn, err := m.Dial(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	h.waitState(t, "w1", StatusUp)
	if h.idleArmed() {
		t.Fatal("idle timer armed while a dialed connection is open")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	h.fireIdle(t)
	h.waitState(t, "w1", StatusDown)
}

func TestStaleSocketUnlinkedAndNonSocketRefused(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})
	sock := filepath.Join(m.sockDir, "w1.sock")

	// Plant a stale socket file (as a crashed previous run would leave).
	if err := os.MkdirAll(m.sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("test setup: stale socket missing: %v", err)
	}

	rt, _ := m.Transport("w1")
	if resp, _, err := get(t, rt, "http://w1/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip over recycled socket path = %v/%v", resp, err)
	}
	_ = m.Close()

	// A non-socket file at the path is never deleted: the attempt fails.
	h2 := newHarness(fr)
	m2 := newTestManager(t, h2, Options{Token: "tok-explicit", SocketDir: m.sockDir})
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt2, _ := m2.Transport("w1")
	_, _, err = get(t, rt2, "http://w1/admin/health")
	if err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("expected refusal to unlink a non-socket file, got %v", err)
	}
	if data, rerr := os.ReadFile(sock); rerr != nil || string(data) != "not a socket" {
		t.Fatalf("planted file must be left alone, got %q err %v", data, rerr)
	}
}

func TestTCPFallbackUsesEphemeralLoopbackPort(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit", TCPTunnels: true})
	rt, _ := m.Transport("w1")
	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("tcp round trip = %v %v %q", err, resp, body)
	}
	fr.mu.Lock()
	fwd := fr.starts[0]
	fr.mu.Unlock()
	if fwd.Network != "tcp" {
		t.Fatalf("forward network = %s, want tcp", fwd.Network)
	}
	host, port, err := net.SplitHostPort(fwd.LocalAddr)
	if err != nil || host != "127.0.0.1" || port == "0" || port == "" {
		t.Fatalf("forward local addr = %s, want 127.0.0.1:<ephemeral>", fwd.LocalAddr)
	}
}

func TestDeadHostNeverBlocksOthers(t *testing.T) {
	fr := &fakeRunner{handler: okHandler(), failByHost: map[string]int{"h1": -1}}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"},
		testDeployment("w1", "h1"), testDeployment("w2", "h2"))

	// The healthy worker round-trips even though w1's host always fails.
	rtGood, _ := m.Transport("w2")
	if resp, _, err := get(t, rtGood, "http://w2/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy worker round trip = %v/%v", resp, err)
	}

	rtBad, _ := m.Transport("w1")
	if _, _, err := get(t, rtBad, "http://w1/admin/health"); err == nil {
		t.Fatal("dead host request should fail")
	}
	h.waitState(t, "w1", StatusDegraded)

	// And the dead host's backoff does not disturb the healthy tunnel.
	if resp, _, err := get(t, rtGood, "http://w2/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy worker round trip after dead host = %v/%v", resp, err)
	}
	states := m.States()
	if states["w1"].Status != StatusDegraded || states["w2"].Status != StatusUp {
		t.Fatalf("states = %+v, want w1 degraded / w2 up", states)
	}
}

func TestUnpinnedHostRefusedBeforeSpawn(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	dir := t.TempDir() // no ouvrier.known_hosts at all
	m := newTestManager(t, h, Options{Token: "tok-explicit", Dir: dir})
	rt, _ := m.Transport("w1")
	_, _, err := get(t, rt, "http://w1/admin/health")
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("expected pinning refusal, got %v", err)
	}
	if got := fr.startCount(); got != 0 {
		t.Fatalf("ssh spawned %d times for an unpinned host, want 0", got)
	}
}

func TestUnknownWorkerAndClose(t *testing.T) {
	fr := &fakeRunner{handler: okHandler()}
	h := newHarness(fr)
	m := newTestManager(t, h, Options{Token: "tok-explicit"})
	if _, err := m.Transport("nope"); !errors.Is(err, ErrUnknownWorker) {
		t.Fatalf("Transport(nope) = %v, want ErrUnknownWorker", err)
	}

	rt, _ := m.Transport("w1")
	if _, _, err := get(t, rt, "http://w1/admin/health"); err != nil {
		t.Fatal(err)
	}
	h.waitState(t, "w1", StatusUp)
	sock := filepath.Join(m.sockDir, "w1.sock")

	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if st := m.States()["w1"]; st.Status != StatusDown {
		t.Fatalf("state after Close = %s, want %s", st.Status, StatusDown)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be unlinked after Close, stat err = %v", err)
	}
	if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrClosed) {
		t.Fatalf("request after Close = %v, want ErrClosed", err)
	}
	if _, err := m.Transport("w1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Transport after Close = %v, want ErrClosed", err)
	}
}

func TestAuthFailedRecoversAfterRecoveryWindowUnderPolling(t *testing.T) {
	// A polling console keeps issuing fail-fast requests while the tunnel is
	// auth_failed. Those must NOT re-arm the full idle window (which would keep
	// auth_failed alive forever); instead the short recovery timer fires once,
	// drops the cached token, and the next open re-fetches the now-fixed token.
	t.Setenv(envnames.AdminToken, "")
	handler := &authHandler{accept: "good-token"} // only the fixed token works
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	// Calls 1-2 yield the bad token (initial + rotation re-fetch -> auth_failed);
	// once the operator fixes the remote, later fetches yield the good token.
	remote := &fakeRemote{envFor: func(call int) string {
		if call <= 2 {
			return dotenvWith("bad-token")
		}
		return dotenvWith("good-token")
	}}
	m := newTestManager(t, h, Options{Remote: remote})
	rt, _ := m.Transport("w1")

	if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("first request = %v, want ErrAuthFailed", err)
	}
	h.waitState(t, "w1", StatusAuthFailed)

	// A burst of polling requests while auth_failed: all fail fast, none fetch,
	// and the recovery timer must survive them (no re-arm, no cancel).
	for i := 0; i < 5; i++ {
		if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("polling request %d = %v, want fail-fast ErrAuthFailed", i, err)
		}
	}
	if got := remote.calls(); got != 2 {
		t.Fatalf("remote .env fetched %d times during polling, want still 2 (no fetch while auth_failed)", got)
	}

	// Fire the auth_failed recovery timer: the tunnel tears down, the cached
	// (bad) token is dropped, and the next poll re-fetches the good token.
	h.fireIdle(t)
	h.waitState(t, "w1", StatusDown)

	resp, body, err := get(t, rt, "http://w1/admin/health")
	if err != nil || resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("request after recovery window = %v %v %q, want 200 ok", err, resp, body)
	}
	if got := remote.calls(); got < 3 {
		t.Fatalf("remote .env fetched %d times after recovery, want a fresh fetch (>=3)", got)
	}
}

func TestResetDropsEverything(t *testing.T) {
	// Reset is the operator escape hatch: it kills the process, unlinks the
	// socket, returns the state to down, and drops the cached token so the next
	// open re-fetches from scratch.
	t.Setenv(envnames.AdminToken, "")
	handler := &authHandler{accept: "tok-1"}
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("tok-1") }}
	m := newTestManager(t, h, Options{Remote: remote})
	rt, _ := m.Transport("w1")

	if resp, _, err := get(t, rt, "http://w1/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip = %v/%v", resp, err)
	}
	h.waitState(t, "w1", StatusUp)
	sock := filepath.Join(m.sockDir, "w1.sock")
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket missing while up: %v", err)
	}

	if err := m.Reset("w1"); err != nil {
		t.Fatalf("Reset = %v, want nil", err)
	}
	if st := m.States()["w1"]; st.Status != StatusDown {
		t.Fatalf("state after Reset = %s, want %s", st.Status, StatusDown)
	}
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket should be unlinked after Reset, stat err = %v", err)
	}

	// The cached token was dropped: the next open re-fetches it fresh.
	if resp, _, err := get(t, rt, "http://w1/admin/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("round trip after Reset = %v/%v", resp, err)
	}
	if got := remote.calls(); got != 2 {
		t.Fatalf("remote .env fetched %d times, want 2 (fresh fetch after Reset dropped the cache)", got)
	}

	// Reset on an unknown worker surfaces ErrUnknownWorker.
	if err := m.Reset("nope"); !errors.Is(err, ErrUnknownWorker) {
		t.Fatalf("Reset(nope) = %v, want ErrUnknownWorker", err)
	}
}

func TestAuthFailedLatchedDropsTokenWhenSupervisorOverwritesState(t *testing.T) {
	// If ssh dies while auth_failed, the supervisor overwrites the state with
	// degraded, so a final-state check would miss the token drop. The latched
	// authFailed bool ensures teardown still drops the cached token.
	t.Setenv(envnames.AdminToken, "")
	handler := &authHandler{accept: ""} // rejects everything -> auth_failed
	fr := &fakeRunner{handler: handler}
	h := newHarness(fr)
	remote := &fakeRemote{envFor: func(int) string { return dotenvWith("bad-token") }}
	m := newTestManager(t, h, Options{Remote: remote})
	rt, _ := m.Transport("w1")

	if _, _, err := get(t, rt, "http://w1/admin/health"); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("first request = %v, want ErrAuthFailed", err)
	}
	h.waitState(t, "w1", StatusAuthFailed)
	if got := remote.calls(); got != 2 {
		t.Fatalf("remote .env fetched %d times, want 2 (initial + re-fetch)", got)
	}

	// ssh dies while auth_failed: the supervisor records degraded, then backoff
	// parks until stop (harness sleep). The latched authFailed must survive the
	// degraded overwrite.
	fr.lastProc(t).die("connection reset", errors.New("exit status 255"))
	h.waitState(t, "w1", StatusDegraded)

	// Tear the tunnel down: teardown must drop the cached token because
	// auth_failed was latched, even though the final pre-teardown state was
	// degraded, not auth_failed.
	m.tunnels["w1"].stop("test teardown")
	h.waitState(t, "w1", StatusDown)

	if tok := m.tunnels["w1"].cachedToken(); tok != "" {
		t.Fatalf("cached token = %q after teardown, want dropped (auth_failed was latched)", tok)
	}
}

func TestManagerRejectsBadNames(t *testing.T) {
	h := newHarness(&fakeRunner{handler: okHandler()})
	for _, bad := range []string{"", "a/b", "..", "-flag", "a b", "héllo"} {
		_, err := newManager([]deploy.Deployment{{Name: bad, Host: "h1"}}, Options{}, h.cfg)
		if err == nil {
			t.Fatalf("name %q accepted, want error", bad)
		}
	}
	_, err := newManager([]deploy.Deployment{
		{Name: "w1", Host: "h1"}, {Name: "w1", Host: "h2"},
	}, Options{}, h.cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate names = %v, want duplicate error", err)
	}
}
