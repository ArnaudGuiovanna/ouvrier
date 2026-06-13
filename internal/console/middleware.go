package console

import (
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
)

// buildHandler assembles the full console handler: the SPA assets at "/" with
// their CSP/X-Frame headers, and the /api/v1 surface behind the auth + DNS-
// rebinding middleware. The order matters — every state-changing or
// token-bearing route lands behind authMiddleware, and authMiddleware itself
// lands behind hostOriginMiddleware so a rebinding attacker is rejected before
// any auth comparison.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// SPA assets. The fileserver is wrapped so every asset response carries the
	// strict CSP + X-Frame-Options: DENY, making the no-CDN rule structural and
	// blocking clickjacking. Assets are public (no token): the token lives in
	// the URL fragment the SPA reads, never gated server-side.
	mux.Handle("/", s.spaHandler())

	// API surface. Registered on a sub-mux so one middleware chain wraps all of
	// /api/v1 uniformly.
	api := http.NewServeMux()
	s.registerAPIRoutes(api)
	mux.Handle("/api/v1/", s.apiMiddleware(api))

	return mux
}

// spaHandler serves the embedded SPA with security headers. The root path
// serves index.html directly (http.FileServer would 301 /index.html -> / and
// loop); real asset paths (app.js, app.css, vendor/...) are served by the
// fileserver.
func (s *Server) spaHandler() http.Handler {
	sub := mustSub(assetsFS, "assets")
	fileServer := http.FileServer(http.FS(sub))
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("console: embed index.html: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Host check applies to assets too: a rebinding attacker must not even
		// load the SPA shell under an attacker-controlled host.
		if !s.hostAllowed(r) {
			http.Error(w, "forbidden: host not allowed", http.StatusForbidden)
			return
		}
		setSPASecurityHeaders(w)
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// setSPASecurityHeaders applies the CSP and framing/clickjacking headers to SPA
// responses. connect-src 'self' confines fetch/EventSource to the console's own
// origin (no CDN, no third-party telemetry); X-Frame-Options: DENY blocks
// framing.
func setSPASecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// apiMiddleware is the chain wrapping every /api/v1 route: DNS-rebinding
// defense (Host allowlist + Origin rejection), then bearer auth, then a
// Cache-Control: no-store on the response. Auth lands before the inner handler,
// so no deploy-capable or proxy route is ever reached unauthenticated.
func (s *Server) apiMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Defense in depth: never let an API response be cached.
		w.Header().Set("Cache-Control", "no-store")

		if !s.hostAllowed(r) {
			http.Error(w, "forbidden: host not allowed", http.StatusForbidden)
			return
		}
		if !s.originAllowed(r) {
			http.Error(w, "forbidden: origin not allowed", http.StatusForbidden)
			return
		}
		if !s.requestAuthorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed enforces the Host-header allowlist: only the loopback names the
// console binds (127.0.0.1, [::1], localhost) on the bound port are accepted.
// This is the primary DNS-rebinding defense — a rebinding attacker's request
// arrives with an attacker-controlled Host that does not match. When
// OUVRIER_CONSOLE_INSECURE=1 the operator has explicitly opted into a
// non-loopback bind, so the Host check is relaxed (they own that risk).
func (s *Server) hostAllowed(r *http.Request) bool {
	if consoleInsecure() {
		return true
	}
	host := r.Host
	if host == "" {
		return false
	}
	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		// No port in Host header; compare hostname directly.
		hostname = host
		port = ""
	}
	_, bindPort, _ := net.SplitHostPort(s.addr)
	if port != "" && bindPort != "" && port != bindPort {
		return false
	}
	return isLoopbackHostname(hostname)
}

// isLoopbackHostname reports whether name is a loopback literal or "localhost".
func isLoopbackHostname(name string) bool {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "localhost") {
		return true
	}
	ip := net.ParseIP(name)
	return ip != nil && ip.IsLoopback()
}

// originAllowed rejects cross-origin requests. With zero cookies the classic
// CSRF vector is gone, but Origin rejection still hardens against a malicious
// page driving the console via fetch (the browser sends Origin on such
// requests). A same-origin request either omits Origin (same-origin GET) or
// sends an Origin whose host is a loopback name — anything else is refused.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Same-origin GETs and EventSource often omit Origin; allowed.
		return true
	}
	if consoleInsecure() {
		return true
	}
	host := originHost(origin)
	if host == "" {
		return false
	}
	hostname, port, err := net.SplitHostPort(host)
	if err != nil {
		hostname = host
		port = ""
	}
	_, bindPort, _ := net.SplitHostPort(s.addr)
	if port != "" && bindPort != "" && port != bindPort {
		return false
	}
	return isLoopbackHostname(hostname)
}

// originHost extracts the host[:port] from an Origin header value
// (scheme://host[:port]). It returns "" for a malformed value or a non-http(s)
// scheme.
func originHost(origin string) string {
	for _, scheme := range []string{"http://", "https://"} {
		if strings.HasPrefix(origin, scheme) {
			rest := origin[len(scheme):]
			if i := strings.IndexAny(rest, "/?#"); i >= 0 {
				rest = rest[:i]
			}
			return rest
		}
	}
	return ""
}

// consoleInsecure reports whether the non-loopback opt-in is set.
func consoleInsecure() bool {
	return strings.TrimSpace(os.Getenv(insecureEnv)) == "1"
}
