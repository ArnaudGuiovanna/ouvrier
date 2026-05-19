package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	runtimeplan "ouvrier/internal/runtime"
	"ouvrier/internal/schema"
)

const shutdownTimeout = 5 * time.Second
const maxHTTPRequestBodyBytes = 1 << 20

type httpStatusResponse struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
}

func newHTTPHandler(nodes []Node) (http.Handler, error) {
	return newHTTPHandlerWithRuntime(nodes, defaultHTTPRuntime())
}

func newHTTPHandlerWithRuntime(nodes []Node, runtime httpRuntime) (http.Handler, error) {
	routes, err := httpRoutesFromNodes(nodes)
	if err != nil {
		return nil, err
	}
	runtime.adminRoutes = routes

	mux := http.NewServeMux()
	registerHTTPAdminRoutes(mux, runtime)
	for _, route := range routes {
		route.runtime = runtime
		if err := registerHTTPRoute(mux, route); err != nil {
			return nil, err
		}
	}
	return mux, nil
}

func registerHTTPRoute(mux *http.ServeMux, route httpRoute) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: HTTP route %s %s: %v", ErrInvalidNode, route.method, route.path, recovered)
		}
	}()

	mux.HandleFunc(route.method+" "+route.path, route.serve)
	return nil
}

func (r httpRoute) serve(w http.ResponseWriter, req *http.Request) {
	if len(r.plan.Steps) > 0 {
		r.servePipeline(w, req)
		return
	}

	switch r.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		if r.plan.Terminal.Async {
			writeJSONStatus(w, http.StatusAccepted, "accepted")
			return
		}
		writeJSONStatus(w, http.StatusOK, "ok")
	case runtimeplan.TerminalPush, runtimeplan.TerminalSink:
		if r.plan.Terminal.Kind == runtimeplan.TerminalSink && r.plan.Terminal.SinkFilePath != "" {
			input, err := buildHTTPRequestInput(req, r.plan.Trigger.Path)
			if err != nil {
				writeJSONStatus(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
				return
			}
			if err := writeFileSink(r.plan.Terminal.SinkFilePath, input); err != nil {
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	default:
		writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
	}
}

func (r httpRoute) servePipeline(w http.ResponseWriter, req *http.Request) {
	input, err := buildHTTPRequestInput(req, r.plan.Trigger.Path)
	if err != nil {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return
	}

	if r.plan.Terminal.Async {
		if !r.tryAcquireWorker() {
			writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
			return
		}
		ctx := context.WithoutCancel(req.Context())
		go func() {
			defer r.releaseWorker()
			_, _ = r.runtime.runPlan(ctx, r.plan, input)
		}()
		writeJSONStatus(w, http.StatusAccepted, "accepted")
		return
	}

	if !r.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	defer r.releaseWorker()

	output, err := r.runtime.runPlan(req.Context(), r.plan, input)
	if err != nil {
		switch {
		case errors.Is(err, errHTTPProviderNotConfigured):
			writeJSONStatus(w, http.StatusServiceUnavailable, "provider_not_configured")
		case errors.Is(err, errHTTPPipelineIncomplete):
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_incomplete")
		default:
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		}
		return
	}
	if err := validateTerminalReplyOutput(r.plan, output); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}

	switch r.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeJSONOutput(w, http.StatusOK, "ok", output)
	case runtimeplan.TerminalPush:
		writeJSONOutput(w, http.StatusAccepted, "accepted", output)
	case runtimeplan.TerminalSink:
		if err := applySinkTerminal(r.plan.Terminal, output); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	default:
		writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
	}
}

func (r httpRoute) tryAcquireWorker() bool {
	if r.workerPool == nil {
		return true
	}
	select {
	case r.workerPool <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r httpRoute) releaseWorker() {
	if r.workerPool == nil {
		return
	}
	<-r.workerPool
}

func readHTTPRequestInput(req *http.Request) (string, error) {
	if req.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxHTTPRequestBodyBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxHTTPRequestBodyBytes {
		return "", errors.New("request body too large")
	}
	return string(body), nil
}

func buildHTTPRequestInput(req *http.Request, routePath string) (string, error) {
	body, err := readHTTPRequestInput(req)
	if err != nil {
		return "", err
	}
	pathParams := httpPathParams(req, routePath)
	return buildHTTPPipelineInput(body, pathParams)
}

func buildHTTPPipelineInput(body string, pathParams map[string]string) (string, error) {
	if len(pathParams) == 0 {
		return body, nil
	}

	input := map[string]any{
		"path_params": pathParams,
	}
	if strings.TrimSpace(body) != "" {
		var decoded any
		if err := json.Unmarshal([]byte(body), &decoded); err == nil {
			input["body"] = decoded
		} else {
			input["body"] = body
		}
	}

	encoded, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func httpPathParams(req *http.Request, routePath string) map[string]string {
	names := httpPathParamNames(routePath)
	if len(names) == 0 {
		return nil
	}
	params := make(map[string]string, len(names))
	for _, name := range names {
		params[name] = req.PathValue(name)
	}
	return params
}

func httpPathParamNames(routePath string) []string {
	segments := strings.Split(routePath, "/")
	names := make([]string, 0)
	for _, segment := range segments {
		if len(segment) < 3 || !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		if name == "" || strings.Contains(name, "...") {
			continue
		}
		names = append(names, name)
	}
	return names
}

func validateTerminalReplyOutput(plan runtimeplan.Plan, output string) error {
	if plan.Terminal.Kind != runtimeplan.TerminalReply || plan.Terminal.ResultSchema == nil {
		return nil
	}
	return schema.ValidateJSON(plan.Terminal.ResultSchema, []byte(output))
}

func applySinkTerminal(terminal runtimeplan.Terminal, output string) error {
	if terminal.SinkFilePath == "" {
		return nil
	}
	return writeFileSink(terminal.SinkFilePath, output)
}

func writeFileSink(path, output string) error {
	return os.WriteFile(path, []byte(output), 0o644)
}

func writeJSONStatus(w http.ResponseWriter, code int, status string) {
	writeJSONOutput(w, code, status, "")
}

func writeJSONOutput(w http.ResponseWriter, code int, status, output string) {
	writeJSON(w, code, httpStatusResponse{Status: status, Output: output})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func serveHTTP(addr string, handler http.Handler) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Handler: handler,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		return <-serveErr
	}
}
