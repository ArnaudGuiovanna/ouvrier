// Package tunnel federates the loopback-only /admin APIs of deployed workers
// over SSH tunnels. A Manager owns one long-lived `ssh -N` forward process
// per worker, forwarding a local unix socket (or, with TCPTunnels, an
// ephemeral loopback port) to the worker's admin listener. Tunnels start
// lazily on first use, retry with jittered backoff when the process dies,
// and close after five idle minutes; admin tokens are fetched from the
// remote shared/.env over SSH and held in memory only. SSH remains the sole
// operator credential: admin ports never leave the host's loopback.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// Sentinel errors. Everything the package returns wraps one of these.
var (
	// ErrTunnel is the package's general failure sentinel.
	ErrTunnel = errors.New("tunnel error")
	// ErrAuthFailed marks a worker whose admin token was rejected even after
	// the single rotation re-fetch.
	ErrAuthFailed = errors.New("tunnel auth failed")
	// ErrClosed is returned once the Manager has been closed.
	ErrClosed = errors.New("tunnel manager closed")
	// ErrUnknownWorker is returned for names absent from the inventory the
	// Manager was built with.
	ErrUnknownWorker = errors.New("unknown worker")
)

// errStopped is the internal signal that an attempt ended because the
// supervisor was told to stop (idle close or Manager.Close), not because the
// tunnel failed.
var errStopped = errors.New("tunnel supervisor stopped")

// Status is one phase of a tunnel's explicit state machine.
type Status string

const (
	// StatusDown means the tunnel is not running: never started, idle-closed,
	// or explicitly closed. A Transport use reopens it lazily.
	StatusDown Status = "down"
	// StatusConnecting means the ssh process is spawned but the local socket
	// is not accepting connections yet.
	StatusConnecting Status = "connecting"
	// StatusUp means the local socket is connectable.
	StatusUp Status = "up"
	// StatusDegraded means the process died or an attempt failed; a backoff
	// retry is pending and the last error is kept.
	StatusDegraded Status = "degraded"
	// StatusAuthFailed means the worker rejected the admin token even after
	// the single rotation re-fetch. The forward itself may still be up;
	// requests fail fast until a short recovery window elapses (the cached
	// token is then dropped and a fresh open re-fetches) or an operator calls
	// Reset.
	StatusAuthFailed Status = "auth_failed"
)

// State is the observable condition of one worker's tunnel. LastError always
// has tokens masked; ssh stderr is surfaced in it verbatim otherwise.
type State struct {
	Status    Status    `json:"status"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since"`
}

// Options configures a Manager.
type Options struct {
	// Dir is the project root holding the committed ouvrier.known_hosts.
	// Every ssh invocation pins host keys against it, exactly like deploy.
	Dir string
	// Token, when non-empty, is the operator-supplied admin token (--token).
	// It takes precedence over the local env var and the remote fetch, and
	// is never written anywhere.
	Token string
	// Identity is an optional ssh identity file passed as -i.
	Identity string
	// TCPTunnels forwards from ephemeral loopback ports instead of unix
	// sockets (the Windows-friendly fallback).
	TCPTunnels bool
	// SocketDir overrides the unix-socket directory (default
	// $XDG_RUNTIME_DIR/ouvrier/tunnels).
	SocketDir string
	// Remote is the SSH seam used for the remote token fetch; nil means the
	// system ssh binary.
	Remote deploy.RemoteRunner
}

// Defaults fixed by design; tests shorten them through managerConfig, never
// through env (same convention as cronLeaseConfig).
const (
	defaultBackoffMin     = 1 * time.Second
	defaultBackoffMax     = 30 * time.Second
	defaultIdleTimeout    = 5 * time.Minute
	defaultDialInterval   = 50 * time.Millisecond
	defaultConnectTimeout = 15 * time.Second
	// defaultAuthRecovery is the short window after which a tunnel stuck in
	// auth_failed drops its cached token and tears down, so a polling console
	// recovers on its own once the operator fixes the remote token instead of
	// re-arming the full idle window on every fail-fast request.
	defaultAuthRecovery = 30 * time.Second
)

// idleTimer is the stoppable handle behind cfg.afterFunc; *time.Timer
// satisfies it in production.
type idleTimer interface{ Stop() bool }

// managerConfig carries the tunnel machinery's timings and seams so
// lifecycle tests are deterministic (injected channels/fakes, no sleeps).
type managerConfig struct {
	runner         tunnelRunner
	backoffMin     time.Duration
	backoffMax     time.Duration
	idleTimeout    time.Duration
	authRecovery   time.Duration
	dialInterval   time.Duration
	connectTimeout time.Duration
	// jitter perturbs a backoff delay (default ±20%); tests use identity.
	jitter func(time.Duration) time.Duration
	// sleep waits d or until stop closes, returning false when stopped.
	sleep func(stop <-chan struct{}, d time.Duration) bool
	// afterFunc schedules the idle close (default time.AfterFunc); tests
	// trigger it manually.
	afterFunc func(d time.Duration, f func()) idleTimer
	now       func() time.Time
	// onState observes every state transition (test hook; may be nil). It is
	// always called outside the tunnel's lock.
	onState func(name string, st State)
}

func defaultManagerConfig() managerConfig {
	return managerConfig{
		runner:         sshRunner{},
		backoffMin:     defaultBackoffMin,
		backoffMax:     defaultBackoffMax,
		idleTimeout:    defaultIdleTimeout,
		authRecovery:   defaultAuthRecovery,
		dialInterval:   defaultDialInterval,
		connectTimeout: defaultConnectTimeout,
		jitter: func(d time.Duration) time.Duration {
			// ±20% full jitter so a fleet of tunnels never retries in lockstep.
			return time.Duration(float64(d) * (0.8 + 0.4*mathrand.Float64()))
		},
		sleep: func(stop <-chan struct{}, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-stop:
				return false
			case <-t.C:
				return true
			}
		},
		afterFunc: func(d time.Duration, f func()) idleTimer { return time.AfterFunc(d, f) },
		now:       time.Now,
	}
}

// Manager federates the admin APIs of a set of deployed workers. Build one
// from the deployments inventory; nothing dials or spawns until the first
// Transport or Dial use per worker (lazy start), and one dead host's backoff
// never blocks another worker's tunnel.
type Manager struct {
	opts    Options
	cfg     managerConfig
	remote  deploy.RemoteRunner
	sockDir string

	mu      sync.Mutex
	closed  bool
	tunnels map[string]*tunnel
}

// NewManager builds a Manager for the given deployments (typically
// deploy.LoadInventory().Deployments). Names must be unique and filename-safe.
func NewManager(deployments []deploy.Deployment, opts Options) (*Manager, error) {
	return newManager(deployments, opts, defaultManagerConfig())
}

func newManager(deployments []deploy.Deployment, opts Options, cfg managerConfig) (*Manager, error) {
	remote := opts.Remote
	if remote == nil {
		remote = deploy.DefaultRemoteRunner()
	}
	m := &Manager{
		opts:    opts,
		cfg:     cfg,
		remote:  remote,
		sockDir: socketDir(opts.SocketDir),
		tunnels: make(map[string]*tunnel, len(deployments)),
	}
	for _, d := range deployments {
		if !validWorkerName(d.Name) {
			return nil, fmt.Errorf("%w: deployment name %q is not usable as a tunnel name (allowed: letters, digits, '.', '_', '-')", ErrTunnel, d.Name)
		}
		if _, dup := m.tunnels[d.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate deployment name %q in inventory", ErrTunnel, d.Name)
		}
		if strings.TrimSpace(d.Host) == "" {
			return nil, fmt.Errorf("%w: deployment %q has no host", ErrTunnel, d.Name)
		}
		m.tunnels[d.Name] = &tunnel{
			m:      m,
			name:   d.Name,
			dep:    d,
			st:     State{Status: StatusDown, Since: cfg.now()},
			notify: make(chan struct{}),
		}
	}
	return m, nil
}

// Transport returns an http.RoundTripper bound to one worker's tunnel. It
// lazily ensures the tunnel is up, refcounts in-flight requests for the idle
// close, injects Authorization: Bearer from the in-memory token, and handles
// the single 401/403 token re-fetch — consumers never see a token. Use any
// http URL; the host is ignored and the request lands on the worker's admin
// listener (e.g. http://<name>/admin/health).
func (m *Manager) Transport(name string) (http.RoundTripper, error) {
	t, err := m.tunnelFor(name)
	if err != nil {
		return nil, err
	}
	return &roundTripper{t: t}, nil
}

// Dial returns a connection through the worker's tunnel, lazily opening it.
// The connection counts as in flight until closed (console reverse proxies
// pass this as a DialContext).
func (m *Manager) Dial(ctx context.Context, name string) (net.Conn, error) {
	t, err := m.tunnelFor(name)
	if err != nil {
		return nil, err
	}
	t.acquire()
	if err := t.ensureUp(ctx); err != nil {
		t.release()
		return nil, err
	}
	t.mu.Lock()
	network, addr := t.network, t.addr
	t.mu.Unlock()
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		t.release()
		return nil, fmt.Errorf("%w: worker %s: dial tunnel socket: %w", ErrTunnel, name, err)
	}
	return &refConn{Conn: conn, release: sync.OnceFunc(t.release)}, nil
}

// States snapshots every worker's tunnel state for observability.
func (m *Manager) States() map[string]State {
	m.mu.Lock()
	tunnels := make([]*tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		tunnels = append(tunnels, t)
	}
	m.mu.Unlock()
	out := make(map[string]State, len(tunnels))
	for _, t := range tunnels {
		t.mu.Lock()
		out[t.name] = t.st
		t.mu.Unlock()
	}
	return out
}

// Close tears down every tunnel: ssh processes killed, sockets unlinked,
// locks released. The Manager is unusable afterwards.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	tunnels := make([]*tunnel, 0, len(m.tunnels))
	for _, t := range m.tunnels {
		tunnels = append(tunnels, t)
	}
	m.mu.Unlock()
	for _, t := range tunnels {
		t.stop("closed")
	}
	return nil
}

// Reset forces a worker's tunnel all the way down for operator-driven
// recovery: it kills the ssh process, unlinks the socket, returns the state to
// down, and drops the cached admin token so the next use re-opens and
// re-fetches from scratch. Unlike the automatic recovery window, Reset is
// immediate and unconditional — the escape hatch when a tunnel is wedged in
// auth_failed or degraded after the remote token was fixed.
func (m *Manager) Reset(name string) error {
	t, err := m.tunnelFor(name)
	if err != nil {
		return err
	}
	t.stop("reset: operator-requested recovery")
	t.dropCachedToken()
	return nil
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *Manager) tunnelFor(name string) (*tunnel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	t, ok := m.tunnels[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q is not in the deployments inventory", ErrUnknownWorker, name)
	}
	return t, nil
}

// tunnel is the per-worker state machine. All mutable fields are guarded by
// mu except the token cache (tokenMu, see token.go), so a slow remote token
// fetch never blocks state observation.
type tunnel struct {
	m    *Manager
	name string
	dep  deploy.Deployment

	mu          sync.Mutex
	st          State
	notify      chan struct{} // closed and remade on every state change
	refs        int           // in-flight requests/conns
	idle        idleTimer     // pending idle close, armed when refs hits 0
	recovery    idleTimer     // pending auth_failed recovery, armed on markAuthFailed
	authFailed  bool          // latched: the tunnel entered auth_failed this lifetime
	supervising bool
	stopCh      chan struct{} // closing tells the supervisor to wind down
	doneCh      chan struct{} // closed when the supervisor has fully torn down
	stopReason  string        // non-empty once a stop was requested
	proc        process
	network     string // "unix" or "tcp" once an attempt has bound
	addr        string
	transport   *http.Transport

	tokenMu sync.Mutex
	token   string // remotely fetched admin token; memory only, never logged
}

// setState publishes a transition, waking ensureUp waiters. Callers must NOT
// hold t.mu; lastErr must already be masked.
func (t *tunnel) setState(status Status, lastErr string) {
	t.mu.Lock()
	st := State{Status: status, LastError: lastErr, Since: t.m.cfg.now()}
	t.st = st
	close(t.notify)
	t.notify = make(chan struct{})
	t.mu.Unlock()
	if hook := t.m.cfg.onState; hook != nil {
		hook(t.name, st)
	}
}

// acquire counts one in-flight use and cancels any pending idle close.
func (t *tunnel) acquire() {
	t.mu.Lock()
	t.refs++
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	t.mu.Unlock()
}

// release drops one in-flight use; when none remain it arms the idle-close
// timer (reset on the next activity).
func (t *tunnel) release() {
	t.mu.Lock()
	t.refs--
	// In auth_failed the recovery timer (armed by markAuthFailed) governs
	// teardown; do not let fail-fast requests re-arm the full idle window, or a
	// polling console would keep auth_failed alive forever.
	if t.refs == 0 && t.supervising && t.stopReason == "" && t.st.Status != StatusAuthFailed {
		t.idle = t.m.cfg.afterFunc(t.m.cfg.idleTimeout, t.idleClose)
	}
	t.mu.Unlock()
}

// idleClose fires from the idle timer: still zero in-flight uses means the
// ssh process is killed and the socket unlinked. The tunnel stays lazily
// reopenable. The refcount is re-checked under the lock because a request
// may have landed between the timer firing and the stop.
func (t *tunnel) idleClose() {
	t.mu.Lock()
	if t.refs == 0 && t.supervising && t.stopReason == "" {
		t.stopReason = fmt.Sprintf("idle: closed after %s without requests", t.m.cfg.idleTimeout)
		close(t.stopCh)
	}
	t.mu.Unlock()
}

// requestStop asks a running supervisor to wind down. It is a no-op when the
// tunnel is not running or a stop is already pending.
func (t *tunnel) requestStop(reason string) {
	t.mu.Lock()
	if t.supervising && t.stopReason == "" {
		t.stopReason = reason
		close(t.stopCh)
	}
	t.mu.Unlock()
}

// stop requests a stop and waits for the supervisor's full teardown.
func (t *tunnel) stop(reason string) {
	t.requestStop(reason)
	t.mu.Lock()
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	if t.recovery != nil {
		t.recovery.Stop()
		t.recovery = nil
	}
	done := t.doneCh
	t.mu.Unlock()
	if done != nil {
		<-done
	}
}

// ensureUp blocks until the tunnel is connectable, lazily starting the
// supervisor. It fails fast — never waiting through a backoff window — when
// the current attempt has already failed (degraded) or auth has failed.
func (t *tunnel) ensureUp(ctx context.Context) error {
	for {
		t.mu.Lock()
		if t.m.isClosed() {
			t.mu.Unlock()
			return ErrClosed
		}
		switch t.st.Status {
		case StatusUp:
			t.mu.Unlock()
			return nil
		case StatusAuthFailed:
			msg := t.st.LastError
			t.mu.Unlock()
			return fmt.Errorf("%w: worker %s: %s", ErrAuthFailed, t.name, msg)
		case StatusDegraded:
			if t.supervising {
				// Retry pending in backoff: fail fast with the last error so
				// one dead host never stalls callers.
				msg := t.st.LastError
				t.mu.Unlock()
				return fmt.Errorf("%w: worker %s: %s", ErrTunnel, t.name, msg)
			}
			t.startSupervisorLocked()
		case StatusDown:
			if !t.supervising {
				t.startSupervisorLocked()
			}
		case StatusConnecting:
			// fall through to wait
		}
		ch := t.notify
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("%w: worker %s: waiting for tunnel: %w", ErrTunnel, t.name, ctx.Err())
		}
	}
}

// startSupervisorLocked launches the per-worker supervisor goroutine.
// Callers hold t.mu.
func (t *tunnel) startSupervisorLocked() {
	t.supervising = true
	t.stopCh = make(chan struct{})
	t.doneCh = make(chan struct{})
	t.stopReason = ""
	t.authFailed = false
	go t.supervise(t.stopCh, t.doneCh)
}

// supervise runs attempts until told to stop, separating each failure with a
// jittered exponential backoff (1s doubling to 30s). It owns the full
// teardown: by the time doneCh closes, the process is dead, the socket
// unlinked, and the flock released.
func (t *tunnel) supervise(stop, done chan struct{}) {
	attempt := 0
	for {
		err := t.runOnce(stop)
		if errors.Is(err, errStopped) {
			break
		}
		attempt++
		t.setState(StatusDegraded, t.maskSecrets(err.Error()))
		// Clamp AFTER jitter so the effective delay never exceeds backoffMax:
		// jitter can perturb the pre-cap schedule above the cap otherwise.
		delay := t.m.cfg.jitter(backoffDelay(attempt, t.m.cfg.backoffMin, t.m.cfg.backoffMax))
		if delay > t.m.cfg.backoffMax {
			delay = t.m.cfg.backoffMax
		}
		if !t.m.cfg.sleep(stop, delay) {
			break
		}
	}

	t.mu.Lock()
	reason := t.stopReason
	if reason == "" {
		reason = "stopped"
	}
	if t.transport != nil {
		// Pooled keep-alive conns point at the dead socket; drop them so a
		// later reopen starts clean.
		defer t.transport.CloseIdleConnections()
	}
	// authFailed is latched for the tunnel's whole lifetime: if ssh died while
	// auth_failed the final state is degraded, not auth_failed, but the cached
	// token was still rejected and must be dropped.
	authFailed := t.authFailed
	if t.recovery != nil {
		t.recovery.Stop()
		t.recovery = nil
	}
	t.supervising = false
	st := State{Status: StatusDown, LastError: reason, Since: t.m.cfg.now()}
	t.st = st
	close(t.notify)
	t.notify = make(chan struct{})
	close(done)
	t.mu.Unlock()
	if authFailed {
		// The cached token was rejected; forget it so the next open
		// re-fetches a possibly rotated one.
		t.dropCachedToken()
	}
	if hook := t.m.cfg.onState; hook != nil {
		hook(t.name, st)
	}
}

// runOnce performs one full attempt: pin the host, prepare the local
// endpoint (unlink + flock for unix sockets, freeport for tcp), spawn ssh,
// wait until the socket accepts, then hold until the process exits or a stop
// is requested. It always returns a non-nil error: the attempt's failure, or
// errStopped.
func (t *tunnel) runOnce(stop <-chan struct{}) error {
	select {
	case <-stop:
		return errStopped
	default:
	}
	t.setState(StatusConnecting, "")

	connect, err := t.connectOpts()
	if err != nil {
		return err
	}
	remoteAddr, err := t.remoteAdminAddr()
	if err != nil {
		return err
	}
	network, addr, lock, err := t.prepareLocal()
	if err != nil {
		return err
	}
	cleanupLocal := func() {
		if network == "unix" {
			_ = os.Remove(addr)
		}
		unlockSocket(lock)
	}

	proc, err := t.m.cfg.runner.Start(connect, Forward{Network: network, LocalAddr: addr, RemoteAddr: remoteAddr})
	if err != nil {
		cleanupLocal()
		return err
	}
	defer cleanupLocal()
	exitCh := make(chan error, 1)
	go func() { exitCh <- proc.Wait() }()

	t.mu.Lock()
	t.proc = proc
	t.network = network
	t.addr = addr
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.proc = nil
		t.mu.Unlock()
	}()

	// Connecting: poll until the local endpoint accepts.
	deadline := time.NewTimer(t.m.cfg.connectTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(t.m.cfg.dialInterval)
	defer tick.Stop()
	for up := false; !up; {
		select {
		case exitErr := <-exitCh:
			return exitFailure(exitErr, proc)
		case <-stop:
			_ = proc.Kill()
			<-exitCh
			return errStopped
		case <-deadline.C:
			_ = proc.Kill()
			<-exitCh
			return fmt.Errorf("%w: worker %s: tunnel socket %s not accepting connections after %s; ssh stderr: %s",
				ErrTunnel, t.name, addr, t.m.cfg.connectTimeout, strings.TrimSpace(proc.Stderr()))
		case <-tick.C:
			conn, dialErr := net.DialTimeout(network, addr, t.m.cfg.dialInterval)
			if dialErr == nil {
				_ = conn.Close()
				up = true
			}
		}
	}

	t.setState(StatusUp, "")
	select {
	case exitErr := <-exitCh:
		return exitFailure(exitErr, proc)
	case <-stop:
		_ = proc.Kill()
		<-exitCh
		return errStopped
	}
}

// exitFailure renders an unexpected ssh exit with its stderr verbatim.
func exitFailure(exitErr error, proc process) error {
	stderr := strings.TrimSpace(proc.Stderr())
	if exitErr == nil {
		return fmt.Errorf("%w: ssh tunnel exited unexpectedly; stderr: %s", ErrTunnel, stderr)
	}
	return fmt.Errorf("%w: ssh tunnel exited: %v; stderr: %s", ErrTunnel, exitErr, stderr)
}

// connectOpts builds the hardened, pinned ssh dial options for this worker.
// Pinning is enforced on every attempt against the committed
// ouvrier.known_hosts, exactly like every deploy invocation.
func (t *tunnel) connectOpts() (deploy.ConnectOpts, error) {
	knownHosts, _, err := deploy.RequirePinnedHost(t.m.opts.Dir, t.dep.Host, t.dep.Port)
	if err != nil {
		return deploy.ConnectOpts{}, fmt.Errorf("%w: worker %s: %w", ErrTunnel, t.name, err)
	}
	return deploy.ConnectOpts{
		Host:       t.dep.Host,
		User:       t.dep.User,
		Port:       t.dep.Port,
		Identity:   t.m.opts.Identity,
		KnownHosts: knownHosts,
	}, nil
}

// remoteAdminAddr resolves the worker's loopback admin listener, defaulting
// to deploy.DefaultAdminAddr the same way `ouvrier deploy` provisions it.
func (t *tunnel) remoteAdminAddr() (string, error) {
	addr := strings.TrimSpace(t.dep.AdminAddr)
	if addr == "" {
		addr = deploy.DefaultAdminAddr
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", fmt.Errorf("%w: worker %s: invalid admin_addr %q: %w", ErrTunnel, t.name, addr, err)
	}
	return addr, nil
}

// prepareLocal readies the local forward endpoint: the 0700 socket dir, the
// flock'd, unlinked-before-spawn unix socket — or a freeport loopback
// address under TCPTunnels (lock is nil then).
func (t *tunnel) prepareLocal() (network, addr string, lock *os.File, err error) {
	if t.m.opts.TCPTunnels {
		addr, err = freePort()
		return "tcp", addr, nil, err
	}
	if err := ensureSocketDir(t.m.sockDir); err != nil {
		return "", "", nil, err
	}
	sock, err := socketPath(t.m.sockDir, t.name)
	if err != nil {
		return "", "", nil, err
	}
	lock, err = lockSocket(sock)
	if err != nil {
		return "", "", nil, err
	}
	if err := unlinkStaleSocket(sock); err != nil {
		unlockSocket(lock)
		return "", "", nil, err
	}
	return "unix", sock, lock, nil
}

// httpTransport lazily builds the per-worker http.Transport whose dials all
// land on the tunnel's current local endpoint.
func (t *tunnel) httpTransport() *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.transport == nil {
		t.transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				t.mu.Lock()
				network, addr := t.network, t.addr
				t.mu.Unlock()
				if network == "" {
					return nil, fmt.Errorf("%w: worker %s: tunnel is not open", ErrTunnel, t.name)
				}
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
	}
	return t.transport
}

// markAuthFailed records a token rejection that survived the single rotation
// re-fetch. It latches authFailed (so teardown drops the cached token even if
// the supervisor later overwrites the state with degraded) and arms a short
// recovery timer: a polling console keeps acquiring/releasing the refcount,
// which would otherwise re-arm the full idle window forever, so instead the
// recovery timer fires once and tears the tunnel down — dropping the cached
// token so the next open re-fetches a possibly fixed one.
func (t *tunnel) markAuthFailed(code int, detail string) {
	t.setState(StatusAuthFailed, t.maskSecrets(fmt.Sprintf("admin request rejected with HTTP %d: %s", code, detail)))
	t.mu.Lock()
	t.authFailed = true
	// Drop any pending idle close: while auth_failed the recovery timer, not
	// the idle window, governs teardown.
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	if t.recovery == nil && t.supervising && t.stopReason == "" {
		t.recovery = t.m.cfg.afterFunc(t.m.cfg.authRecovery, t.authRecover)
	}
	t.mu.Unlock()
}

// authRecover fires from the recovery timer: it winds the supervisor down with
// a recovery reason. Teardown then drops the cached token (authFailed is
// latched), leaving the tunnel down and lazily reopenable, so the next request
// re-fetches the token instead of staying stuck in auth_failed.
func (t *tunnel) authRecover() {
	t.requestStop("auth_failed: recovery window elapsed; dropping cached token")
}

// maskErr wraps err so its message cannot leak the worker's token.
func (t *tunnel) maskErr(err error) error {
	if err == nil {
		return nil
	}
	t.tokenMu.Lock()
	cached := t.token
	t.tokenMu.Unlock()
	return deploy.MaskTokenErr(deploy.MaskTokenErr(err, cached), strings.TrimSpace(t.m.opts.Token))
}

// backoffDelay is the pre-jitter exponential schedule: min, 2min, 4min, ...
// capped at max (1s -> 30s with the defaults).
func backoffDelay(attempt int, min, max time.Duration) time.Duration {
	d := min
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// roundTripper is Manager.Transport's per-worker http.RoundTripper. It keeps
// tokens entirely out of consumers: ensure up, count the request in flight
// until its body is closed, inject Authorization, and absorb the single
// 401/403 token re-fetch.
type roundTripper struct {
	t *tunnel
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t := rt.t
	t.acquire()
	release := sync.OnceFunc(t.release)
	resp, err := rt.roundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

func (rt *roundTripper) roundTrip(req *http.Request) (*http.Response, error) {
	t := rt.t
	ctx := req.Context()
	if err := t.ensureUp(ctx); err != nil {
		return nil, err
	}
	token, src, err := t.adminToken(ctx, "")
	if err != nil {
		return nil, t.maskErr(err)
	}
	resp, err := rt.send(req, token, false)
	if err != nil {
		return nil, t.maskErr(err)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}

	// Token rejected. A request whose body cannot be replayed is surfaced
	// untouched (no retry, no condemnation): we never re-attempt a half-sent
	// body, and never auth_fail on a request we couldn't cleanly evaluate.
	if req.Body != nil && req.GetBody == nil {
		return resp, nil
	}
	code := resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()

	// The recovery depends on where the token came from:
	//   - explicit --token: the operator deliberately chose it, so a rejection
	//     is terminal (auth_failed) with no silent remote override;
	//   - remote-fetched: re-fetch once (token rotation on the host) and retry;
	//   - env fallback: fall through to a remote fetch once and retry, since
	//     the local env var was only ever a stand-in for the remote value.
	if src == sourceOptions {
		t.markAuthFailed(code, "explicit --token rejected")
		return nil, fmt.Errorf("%w: worker %s: HTTP %d and the explicit --token was rejected", ErrAuthFailed, t.name, code)
	}

	// stale tells remoteToken which value to bypass the cache for. For a
	// rejected remote token that is the token itself (force a rotation
	// re-fetch); for a rejected env token there is no cached remote token to
	// distrust, so any cached/fresh remote value is acceptable.
	stale := ""
	if src == sourceRemote {
		stale = token
	}
	fresh, err := t.remoteToken(ctx, stale)
	if err != nil {
		t.markAuthFailed(code, "token re-fetch failed: "+err.Error())
		return nil, fmt.Errorf("%w: worker %s: HTTP %d and token re-fetch failed: %w", ErrAuthFailed, t.name, code, t.maskErr(err))
	}
	if fresh == token {
		t.markAuthFailed(code, "token re-fetch returned the same token")
		return nil, fmt.Errorf("%w: worker %s: HTTP %d and the re-fetched token is unchanged", ErrAuthFailed, t.name, code)
	}
	resp, err = rt.send(req, fresh, true)
	if err != nil {
		return nil, t.maskErr(err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.markAuthFailed(resp.StatusCode, "rejected again after token re-fetch")
	}
	return resp, nil
}

// send clones the request, injects the bearer token, and dispatches it
// through the tunnel's transport. replay re-materializes the body via
// GetBody for the post-re-fetch retry.
func (rt *roundTripper) send(req *http.Request, token string, replay bool) (*http.Response, error) {
	r := req.Clone(req.Context())
	if replay && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("%w: worker %s: replay request body: %w", ErrTunnel, rt.t.name, err)
		}
		r.Body = body
	}
	if r.URL.Scheme == "" {
		r.URL.Scheme = "http"
	}
	if r.URL.Host == "" {
		r.URL.Host = rt.t.name
	}
	r.Header.Set("Authorization", "Bearer "+token)
	return rt.t.httpTransport().RoundTrip(r)
}

// releasingBody decrements the tunnel's in-flight refcount when the response
// body is closed, so streaming responses (SSE tails) hold the tunnel open.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// refConn ties a Manager.Dial connection into the idle-close refcount.
type refConn struct {
	net.Conn
	release func()
}

func (c *refConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
