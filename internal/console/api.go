package console

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/adminapi"
	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tunnel"
)

// overviewTimeout bounds each per-worker overview fan-out call. A slow or down
// target yields a partial result rather than stalling the whole page.
const overviewTimeout = 5 * time.Second

// deployFunc is the deploy-engine seam so the deploy endpoint is testable
// without a real SSH host. Production uses deploy.DeployEnvironment.
type deployFunc func(ctx context.Context, opts deploy.EnvOpts, progress deploy.ProgressWriter) error

var defaultDeployFunc deployFunc = deploy.DeployEnvironment

// registerAPIRoutes wires the /api/v1 surface. Every route here is reached only
// after apiMiddleware has enforced Host/Origin + bearer auth.
func (s *Server) registerAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/fleet", s.handleFleet)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/environments", s.handleEnvironments)
	mux.HandleFunc("POST /api/v1/workers/{name}/deploy", s.handleDeploy)
	mux.HandleFunc("POST /api/v1/workers/{name}/reset", s.handleReset)
	// The catch-all admin proxy. The {rest...} wildcard captures the remaining
	// path; the allowlist (admin_allowlist.go) decides method+path before any
	// byte reaches the worker.
	mux.HandleFunc("/api/v1/workers/{name}/admin/{rest...}", s.handleAdminProxy)
}

// writeJSON encodes v as JSON with no-store already set by the middleware.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- fleet -----------------------------------------------------------------

// fleetWorker is one row of the fleet overview: the inventory record plus the
// live tunnel state. No token-shaped field is ever included.
type fleetWorker struct {
	Name        string       `json:"name"`
	Host        string       `json:"host"`
	User        string       `json:"user,omitempty"`
	Service     string       `json:"service,omitempty"`
	DeployedAt  time.Time    `json:"deployed_at"`
	Result      string       `json:"result,omitempty"`
	TunnelState tunnel.State `json:"tunnel"`
}

// handleFleet returns the inventory plus per-worker tunnel state. It maps to the
// CLI `ouvrier fleet ls` (inventory) + tunnel States() observability.
func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	deployments, err := s.deployments()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mgr, err := s.manager()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	states := mgr.States()

	workers := make([]fleetWorker, 0, len(deployments))
	for _, d := range deployments {
		workers = append(workers, fleetWorker{
			Name:        d.Name,
			Host:        d.Host,
			User:        d.User,
			Service:     d.Service,
			DeployedAt:  d.DeployedAt,
			Result:      d.Result,
			TunnelState: states[d.Name],
		})
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].Name < workers[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

// handleEnvironments lists the deployable environments from pip.yaml for the
// deploy view picker. Maps to `ouvrier deploy <env>` capability discovery.
func (s *Server) handleEnvironments(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"environments": listEnvNames(s.dir)})
}

// ---- overview --------------------------------------------------------------

// overviewWorker is one target's status in the fan-out. Status holds the
// decoded /admin/status body on success; Error holds a message on failure
// (partial results: one down target never fails the whole response).
type overviewWorker struct {
	Name   string          `json:"name"`
	OK     bool            `json:"ok"`
	Status json.RawMessage `json:"status,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// handleOverview fans out GET /admin/status across every worker concurrently
// with a 5s per-target timeout, returning partial results. Maps to the CLI
// `ouvrier status --all`.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	deployments, err := s.deployments()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mgr, err := s.manager()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}

	results := make([]overviewWorker, len(deployments))
	var wg sync.WaitGroup
	for i, d := range deployments {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			results[i] = s.overviewOne(r.Context(), mgr, name)
		}(i, d.Name)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"workers": results})
}

// overviewOne calls one worker's /admin/status over its tunnel transport under
// the per-target timeout. The transport injects the admin token server-side; the
// tokenless adminapi client never sees it.
func (s *Server) overviewOne(ctx context.Context, mgr Manager, name string) overviewWorker {
	res := overviewWorker{Name: name}
	rt, err := mgr.Transport(name)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, overviewTimeout)
	defer cancel()

	client := adminapi.NewClient(&http.Client{Transport: rt}, "http://"+name, "")
	var raw json.RawMessage
	if err := client.GetJSON(cctx, "/admin/status", &raw); err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	res.Status = raw
	return res
}

// ---- reset -----------------------------------------------------------------

// handleReset forces a worker's tunnel down + drops its cached token, the
// operator recovery escape hatch. Maps to a fleet recovery action; it changes
// only tunnel state, never the worker.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	mgr, err := s.manager()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resetter, ok := mgr.(interface{ Reset(string) error })
	if !ok {
		writeAPIError(w, http.StatusNotImplemented, "tunnel reset not supported")
		return
	}
	if err := resetter.Reset(name); err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset", "worker": name})
}

// ---- deploy ----------------------------------------------------------------

// handleDeploy streams a release-layout deploy as JSONL progress lines. The
// environment is taken from the {name} path segment (the console deploys a
// pip.yaml environment, mirroring `ouvrier deploy <env>`). Each line is a JSON
// object {"stream":"out|err","line":"..."}; a final {"done":true,"error":"..."}
// closes the stream. Maps to the CLI `ouvrier deploy <env>`.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	envName := r.PathValue("name")
	opts, err := resolveEnvDeployOpts(s.dir, envName)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	// A line-buffered writer per stream emits one JSONL record per line so the
	// browser sees progress as it happens (FlushInterval-equivalent for the
	// deploy path).
	enc := &jsonlProgress{w: w, flusher: flusher}
	outW := &jsonlLineWriter{p: enc, stream: "out"}
	errW := &jsonlLineWriter{p: enc, stream: "err"}

	runErr := s.deployFn()(r.Context(), opts, deploy.ProgressWriter{Out: outW, Err: errW})
	outW.flush()
	errW.flush()

	done := map[string]any{"done": true}
	if runErr != nil {
		done["error"] = runErr.Error()
	}
	enc.writeRecord(done)
}

// deployFn returns the deploy seam (test override or production default).
func (s *Server) deployFn() deployFunc {
	if s.deploy != nil {
		return s.deploy
	}
	return defaultDeployFunc
}

// jsonlProgress writes newline-delimited JSON records and flushes each one so
// the browser renders deploy progress live.
type jsonlProgress struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func (p *jsonlProgress) writeRecord(rec any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = p.w.Write(append(b, '\n'))
	p.flusher.Flush()
}

// jsonlLineWriter buffers bytes into lines and emits one JSONL record per line.
type jsonlLineWriter struct {
	p      *jsonlProgress
	stream string
	buf    strings.Builder
}

func (lw *jsonlLineWriter) Write(b []byte) (int, error) {
	for _, c := range b {
		if c == '\n' {
			lw.emit()
			continue
		}
		lw.buf.WriteByte(c)
	}
	return len(b), nil
}

func (lw *jsonlLineWriter) emit() {
	line := lw.buf.String()
	lw.buf.Reset()
	lw.p.writeRecord(map[string]string{"stream": lw.stream, "line": line})
}

func (lw *jsonlLineWriter) flush() {
	if lw.buf.Len() > 0 {
		lw.emit()
	}
}

// ---- admin proxy -----------------------------------------------------------

// handleAdminProxy is the reverse proxy to a worker's /admin/* listener over
// its SSH tunnel. The allowlist (admin_allowlist.go) is checked first: only
// known-safe method+path combinations are forwarded; everything else is 403.
// The tunnel manager's transport injects the admin token server-side, so the
// browser never sends or receives it. FlushInterval=-1 streams SSE events as
// they arrive.
func (s *Server) handleAdminProxy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rest := r.PathValue("rest")
	adminPath := "/admin/" + rest

	if !adminProxyAllowed(r.Method, adminPath) {
		writeAPIError(w, http.StatusForbidden, fmt.Sprintf("admin route not allowed from console: %s %s", r.Method, adminPath))
		return
	}

	mgr, err := s.manager()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rt, err := mgr.Transport(name)
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
		// FlushInterval=-1 flushes after every write, so SSE/JSONL event
		// streams reach the browser per event rather than buffered.
		FlushInterval: -1,
		Transport:     rt,
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The tunnel transport ignores the host; we set a stable one so the
			// upstream sees a clean request. The query string (after_id, format,
			// follow, etc.) is preserved.
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = name
			pr.Out.URL.Path = adminPath
			// Preserve admin query params (after_id, format, follow, …) but drop
			// the console's own access_token so the session secret never reaches
			// the worker, even on a hand-crafted proxy URL.
			q := r.URL.Query()
			q.Del("access_token")
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = name
			// Strip the console's own Authorization (the session token): the
			// tunnel transport injects the real admin token. The browser's
			// session token must never reach the worker.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, perr error) {
			writeAPIError(rw, http.StatusBadGateway, "worker unreachable: "+perr.Error())
		},
		ModifyResponse: func(resp *http.Response) error {
			// Defense in depth: the upstream admin listener never sets these,
			// but guarantee no Set-Cookie or token-ish header crosses back to
			// the browser.
			resp.Header.Del("Set-Cookie")
			return nil
		},
	}
	proxy.ServeHTTP(w, r)
}

// ---- events fan-in ---------------------------------------------------------

// handleEvents is a Server-Sent-Events fan-in of every worker's
// /admin/events?format=jsonl&follow=true stream. Each event is re-tagged with
// the worker name and forwarded to the browser as one SSE message. Down targets
// surface a synthetic console.worker_unreachable event so the operator sees the
// gap rather than silence. Maps to the CLI `ouvrier logs`/`trace` fleet mode.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	deployments, err := s.deployments()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	mgr, err := s.manager()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sink := &sseSink{w: w, flusher: flusher}
	ctx := r.Context()

	var wg sync.WaitGroup
	for _, d := range deployments {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			s.tailWorkerEvents(ctx, mgr, name, sink)
		}(d.Name)
	}
	wg.Wait()
}

// tailWorkerEvents follows one worker's JSONL event stream and forwards each
// line to the shared SSE sink, tagged with the worker name. On any failure
// (tunnel down, mid-stream error) it emits a synthetic
// console.worker_unreachable event and returns; the browser reconnects via the
// per-worker after_id cursor it tracks.
func (s *Server) tailWorkerEvents(ctx context.Context, mgr Manager, name string, sink *sseSink) {
	rt, err := mgr.Transport(name)
	if err != nil {
		sink.unreachable(name, err.Error())
		return
	}
	url := "http://" + name + "/admin/events?format=jsonl&follow=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		sink.unreachable(name, err.Error())
		return
	}
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		sink.unreachable(name, err.Error())
		return
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode/100 != 2 {
		sink.unreachable(name, fmt.Sprintf("admin events returned HTTP %d", resp.StatusCode))
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		sink.event(name, json.RawMessage(line))
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sink.unreachable(name, err.Error())
	}
}

// sseSink serializes concurrent writes from every worker's tail goroutine into
// one SSE response.
type sseSink struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

// event forwards a worker event as an SSE message tagged with the worker name.
// The data payload wraps the worker's raw event JSON so the browser can route
// it and maintain a per-worker after_id cursor.
func (s *sseSink) event(worker string, raw json.RawMessage) {
	s.write(map[string]any{"worker": worker, "event": raw})
}

// unreachable emits the synthetic console.worker_unreachable event.
func (s *sseSink) unreachable(worker, reason string) {
	s.write(map[string]any{
		"worker": worker,
		"event": map[string]any{
			"kind":   "console.worker_unreachable",
			"at":     time.Now().UTC(),
			"reason": reason,
		},
	})
}

func (s *sseSink) write(payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flusher.Flush()
}
