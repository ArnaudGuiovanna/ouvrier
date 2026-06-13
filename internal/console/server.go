// Package console is the loopback-only operator web console for an Ouvrier
// fleet. It is strictly a thin layer over internal/deploy (deploy + inventory),
// internal/adminapi (the GET fan-out), and internal/tunnel (the per-worker
// SSH-forwarded admin proxy). It owns no business logic of its own: handlers
// decode the request, call into those packages, and encode the result.
//
// Security posture (see the v0.3 design "Web console" section):
//
//   - Bind refuses any non-loopback address unless OUVRIER_CONSOLE_INSECURE=1.
//   - A random 256-bit per-session token is generated at startup, printed in
//     the console URL as a #fragment (so it never lands in server logs or a
//     Referer header), held only in browser memory, and sent back as
//     Authorization: Bearer on every /api/v1 call. The compare is constant-time.
//   - Zero cookies (CSRF is structurally impossible), Host-header allowlist and
//     Origin rejection (DNS-rebinding defense), Cache-Control: no-store on API
//     responses, and a strict CSP + X-Frame-Options: DENY on the SPA.
//   - The admin token fetched by the tunnel manager NEVER reaches the browser:
//     the reverse proxy injects it server-side via the manager's transport.
package console

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// DefaultAddr is the loopback bind address used when --addr / OUVRIER_CONSOLE_ADDR
// is unset.
const DefaultAddr = "127.0.0.1:7333"

// insecureEnv, when "1", lets the console bind a non-loopback address. It
// mirrors the runtime's admin-exposure override and is never the default.
const insecureEnv = "OUVRIER_CONSOLE_INSECURE"

// Options configures a console Server.
type Options struct {
	// Addr is the bind address; empty means DefaultAddr. A non-loopback addr is
	// refused unless OUVRIER_CONSOLE_INSECURE=1.
	Addr string
	// Dir is the project root holding ouvrier.known_hosts and pip.yaml; the
	// tunnel manager and the deploy flow both pin host keys against it. Empty
	// means ".".
	Dir string
	// FleetPath overrides the deployments inventory location (--fleet /
	// OUVRIER_FLEET_PATH). Empty resolves via deploy.InventoryPath().
	FleetPath string
	// Token is the operator-supplied admin token (--token), forwarded to the
	// tunnel manager. Empty means the manager fetches it over SSH.
	Token string

	// newManager is the tunnel-manager seam (tests inject a fake that reaches an
	// httptest admin server without spawning ssh). Nil means the real
	// SSH-backed tunnel.Manager.
	newManager func(deployments []deploy.Deployment, opts tunnel.Options) (Manager, error)
	// sessionToken overrides the generated session token (tests). Empty means a
	// fresh random 256-bit token.
	sessionToken string
	// deploy overrides the deploy engine entrypoint (tests). Nil means
	// deploy.DeployEnvironment.
	deploy deployFunc
}

// Server is the console HTTP server: a single mux wrapping the SPA assets and
// the /api/v1 surface behind the security middleware.
type Server struct {
	addr      string
	dir       string
	fleetPath string
	token     string // operator-supplied admin token (--token)

	sessionToken string
	newManager   func(deployments []deploy.Deployment, opts tunnel.Options) (Manager, error)
	deploy       deployFunc // test seam; nil means deploy.DeployEnvironment

	mu  sync.Mutex // guards mgr
	mgr Manager

	handler http.Handler
}

// NewServer builds a console server. It resolves the bind address, enforces the
// loopback rule, generates the per-session token, and assembles the handler.
// The tunnel manager is built lazily per request batch from the live inventory
// (so adding a worker to the inventory does not require a console restart) —
// except that NewServer validates the inventory is readable up front.
func NewServer(opts Options) (*Server, error) {
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		addr = DefaultAddr
	}
	if err := checkConsoleExposure(addr); err != nil {
		return nil, err
	}

	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = "."
	}

	sessionToken := opts.sessionToken
	if sessionToken == "" {
		tok, err := randomToken()
		if err != nil {
			return nil, err
		}
		sessionToken = tok
	}

	newMgr := opts.newManager
	if newMgr == nil {
		newMgr = func(deployments []deploy.Deployment, mopts tunnel.Options) (Manager, error) {
			return tunnel.NewManager(deployments, mopts)
		}
	}

	s := &Server{
		addr:         addr,
		dir:          dir,
		fleetPath:    strings.TrimSpace(opts.FleetPath),
		token:        strings.TrimSpace(opts.Token),
		sessionToken: sessionToken,
		newManager:   newMgr,
		deploy:       opts.deploy,
	}
	s.handler = s.buildHandler()
	return s, nil
}

// Addr reports the resolved bind address.
func (s *Server) Addr() string { return s.addr }

// SessionToken reports the per-session bearer token (used to render the
// console URL fragment).
func (s *Server) SessionToken() string { return s.sessionToken }

// Handler exposes the assembled handler for tests (httptest.NewServer) and any
// embedding caller.
func (s *Server) Handler() http.Handler { return s.handler }

// Manager returns the live tunnel manager, building it from the current
// inventory on first use. The console keeps one manager for its lifetime: it
// refcounts and idle-closes tunnels itself, so a long-lived console does not
// hold ssh processes open for idle workers.
func (s *Server) manager() (Manager, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mgr != nil {
		return s.mgr, nil
	}
	deployments, err := s.deployments()
	if err != nil {
		return nil, err
	}
	mgr, err := s.newManager(deployments, tunnel.Options{Dir: s.dir, Token: s.token})
	if err != nil {
		return nil, err
	}
	s.mgr = mgr
	return mgr, nil
}

// Close tears down the tunnel manager (ssh processes, sockets).
func (s *Server) Close() error {
	s.mu.Lock()
	mgr := s.mgr
	s.mu.Unlock()
	if mgr != nil {
		return mgr.Close()
	}
	return nil
}

// Listen binds the configured address. The CLI uses Listen + ServeListener
// (rather than a one-shot Serve) so it can print the concrete bound port — and
// thus a clickable URL — before serving, which matters when --addr uses :0.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("console: bind %s: %w", s.addr, err)
	}
	return ln, nil
}

// ServeListener serves on an already-bound listener until it errors or closes.
func (s *Server) ServeListener(ln net.Listener) error {
	srv := &http.Server{Handler: s.handler}
	return srv.Serve(ln)
}

// deployments loads and de-duplicates the inventory (one entry per name, first
// row wins), matching the fleet CLI's resolveFleetTargets collapse rule so the
// tunnel manager — which keys on name and rejects duplicates — never sees a
// duplicate.
func (s *Server) deployments() ([]deploy.Deployment, error) {
	path := s.fleetPath
	if path == "" {
		p, err := deploy.InventoryPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	inv, err := deploy.LoadInventory(path)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(inv.Deployments))
	out := make([]deploy.Deployment, 0, len(inv.Deployments))
	for _, d := range inv.Deployments {
		if _, ok := seen[d.Name]; ok {
			continue
		}
		seen[d.Name] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

// checkConsoleExposure refuses a non-loopback bind unless the operator opts in
// with OUVRIER_CONSOLE_INSECURE=1. This mirrors the runtime's checkAdminExposure
// so the console cannot be accidentally exposed to the network.
func checkConsoleExposure(addr string) error {
	if isLoopbackAddr(addr) {
		return nil
	}
	if strings.TrimSpace(os.Getenv(insecureEnv)) == "1" {
		return nil
	}
	return fmt.Errorf(
		"console: refusing to bind %q: it is reachable from the network and the console serves unauthenticated assets plus a token-bearing admin proxy; bind a loopback address (127.0.0.1:7333) or set %s=1 to override",
		addr, insecureEnv)
}

// isLoopbackAddr reports whether addr's host is a loopback address or
// "localhost". An empty or invalid host is treated as non-loopback (fail safe).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.TrimSpace(addr)
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// randomToken returns a 256-bit cryptographically-random token, hex-encoded.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("console: generate session token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// requestAuthorized reports whether r carries the session token. Every route
// accepts it as Authorization: Bearer. The SSE events stream additionally
// accepts it as the access_token query parameter, because the browser
// EventSource API cannot set request headers; the token is still the same
// 256-bit secret and is still constant-time compared. Allowing the query param
// only on the GET events stream keeps it off mutating routes (where it could
// leak via Referer on a redirect) and never gates the deploy/proxy POSTs.
func (s *Server) requestAuthorized(r *http.Request) bool {
	if s.constantTimeMatch(r.Header.Get("Authorization")) {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/events" {
		if tok := r.URL.Query().Get("access_token"); tok != "" {
			return s.tokenEqual(tok)
		}
	}
	return false
}

// constantTimeMatch reports whether the bearer token in the Authorization
// header equals the session token, comparing in constant time.
func (s *Server) constantTimeMatch(authHeader string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	return s.tokenEqual(strings.TrimSpace(authHeader[len(prefix):]))
}

// tokenEqual constant-time compares got against the session token. The token
// length is fixed (64 hex chars) and not secret, so the length short-circuit
// leaks nothing.
func (s *Server) tokenEqual(got string) bool {
	want := s.sessionToken
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// staticFS is the SPA asset tree, rooted at assets/ via the embed in assets.go.
func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(fmt.Sprintf("console: embed sub %q: %v", dir, err))
	}
	return sub
}

// drainAndClose drains and closes a response body so the underlying connection
// can be reused, used after proxied/streamed responses on error paths.
func drainAndClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
