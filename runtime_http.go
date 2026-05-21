package ovr

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	"ouvrier/internal/events"
	runtimeplan "ouvrier/internal/runtime"
	"ouvrier/internal/schema"
	"ouvrier/internal/state"
)

const shutdownTimeout = 5 * time.Second
const maxHTTPRequestBodyBytes = 1 << 20
const directReplyOKOutput = `{"status":"ok"}`

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
	if err := validateHTTPSignatureConfig(routes); err != nil {
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
	input, ok := r.prepareRequestInput(w, req)
	if !ok {
		return
	}

	switch r.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		if r.plan.Terminal.Async {
			writeJSONStatus(w, http.StatusAccepted, "accepted")
			return
		}
		if err := r.runtime.validateDirectTerminalReplyOutput(req.Context(), r.plan); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONStatus(w, http.StatusOK, "ok")
	case runtimeplan.TerminalPush:
		if r.plan.Terminal.PushWebhookURL != "" {
			if err := applyPushTerminal(req.Context(), r.plan.Terminal, input); err != nil {
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	case runtimeplan.TerminalSink:
		if r.plan.Terminal.SinkFilePath != "" || r.plan.Terminal.SinkLog {
			if err := r.runtime.applySinkTerminal(req.Context(), r.plan.Terminal, planRunResult{Output: input}, "input"); err != nil {
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
	input, ok := r.prepareRequestInput(w, req)
	if !ok {
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

	result, err := r.runtime.runPlanResult(req.Context(), r.plan, input)
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
	if err := r.runtime.validateObservedTerminalReplyOutput(req.Context(), r.plan, result); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}
	output := result.Output

	switch r.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeJSONOutput(w, http.StatusOK, "ok", output)
	case runtimeplan.TerminalPush:
		if err := applyPushTerminal(req.Context(), r.plan.Terminal, output); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONOutput(w, http.StatusAccepted, "accepted", output)
	case runtimeplan.TerminalSink:
		if err := r.runtime.applySinkTerminal(req.Context(), r.plan.Terminal, result, "output"); err != nil {
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

func validateHTTPSignatureConfig(routes []httpRoute) error {
	for _, route := range routes {
		env := strings.TrimSpace(route.plan.Trigger.SignatureEnv)
		if env == "" {
			continue
		}
		if strings.TrimSpace(os.Getenv(env)) == "" {
			return fmt.Errorf("%w: VerifySignature secret env var %s is not set", ErrInvalidNode, env)
		}
	}
	return nil
}

func (r httpRoute) prepareRequestInput(w http.ResponseWriter, req *http.Request) (string, bool) {
	body, err := readHTTPRequestInput(req)
	if err != nil {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return "", false
	}
	if !r.verifyRequestSignature(w, req, []byte(body)) {
		return "", false
	}
	input, err := buildHTTPPipelineInput(body, httpPathParams(req, r.plan.Trigger.Path))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_request")
		return "", false
	}
	return input, true
}

func (r httpRoute) verifyRequestSignature(w http.ResponseWriter, req *http.Request, payload []byte) bool {
	env := strings.TrimSpace(r.plan.Trigger.SignatureEnv)
	headerName := strings.TrimSpace(r.plan.Trigger.SignatureHeader)
	if env == "" && headerName == "" {
		return true
	}
	headerValue := strings.TrimSpace(req.Header.Get(headerName))
	if headerValue == "" {
		writeJSONStatus(w, http.StatusUnauthorized, "signature_missing")
		return false
	}
	secret := os.Getenv(env)
	if secret == "" {
		writeJSONStatus(w, http.StatusInternalServerError, "signature_secret_missing")
		return false
	}
	if !validHMACSHA256Signature(secret, payload, headerValue) {
		writeJSONStatus(w, http.StatusForbidden, "signature_invalid")
		return false
	}
	return true
}

func validHMACSHA256Signature(secret string, payload []byte, rawSignature string) bool {
	provided, ok := decodeHMACSHA256Signature(rawSignature)
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return hmac.Equal(provided, mac.Sum(nil))
}

func decodeHMACSHA256Signature(raw string) ([]byte, bool) {
	signature := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(signature), "sha256=") {
		signature = signature[len("sha256="):]
	}
	decoded, err := hex.DecodeString(signature)
	if err != nil || len(decoded) != sha256.Size {
		return nil, false
	}
	return decoded, true
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

func (rt httpRuntime) validateDirectTerminalReplyOutput(ctx context.Context, plan runtimeplan.Plan) error {
	return rt.validateObservedTerminalReplyOutput(ctx, plan, planRunResult{Output: directReplyOKOutput})
}

func (rt httpRuntime) validateObservedTerminalReplyOutput(ctx context.Context, plan runtimeplan.Plan, result planRunResult) error {
	if terminalReplySchemaAlreadyValidated(plan) {
		return nil
	}
	if plan.Terminal.Kind != runtimeplan.TerminalReply || plan.Terminal.ResultSchema == nil {
		return nil
	}
	if err := schema.ValidateJSON(plan.Terminal.ResultSchema, []byte(result.Output)); err != nil {
		recordErr := rt.recordTerminalSchemaViolation(ctx, plan.Terminal.ResultSchema, result, err)
		return errors.Join(err, recordErr)
	}
	return rt.emitRuntimeEvent(ctx, result, events.EventSchemaValidationPassed, map[string]any{
		"schema": plan.Terminal.ResultSchema.Name,
	})
}

func terminalReplySchemaAlreadyValidated(plan runtimeplan.Plan) bool {
	if plan.Terminal.Kind != runtimeplan.TerminalReply || plan.Terminal.ResultSchema == nil || len(plan.Steps) == 0 {
		return false
	}
	lastStep := plan.Steps[len(plan.Steps)-1]
	if lastStep.ResultSchema == nil {
		return false
	}
	return compatibleResultSchemas(lastStep.ResultSchema, plan.Terminal.ResultSchema)
}

func (rt httpRuntime) recordTerminalSchemaViolation(ctx context.Context, contract *runtimeplan.ResultSchema, result planRunResult, validationErr error) error {
	violation := state.SchemaViolation{
		SchemaName: contract.Name,
		Error:      validationErr.Error(),
	}
	if result.HasSession {
		violation.ExecID = result.Session.ExecID
		violation.SessionID = result.Session.SessionID
	}
	var err error
	if rt.stateStore != nil {
		_, err = rt.stateStore.AddSchemaViolation(ctx, violation)
	}
	emitErr := rt.emitRuntimeEvent(ctx, result, events.EventSchemaViolation, map[string]any{
		"schema": contract.Name,
		"error":  validationErr.Error(),
	})
	return errors.Join(err, emitErr)
}

func (rt httpRuntime) emitRuntimeEvent(ctx context.Context, result planRunResult, kind events.EventKind, payload map[string]any) error {
	if result.HasSession {
		return rt.emitSessionEvent(ctx, result.Session, kind, payload)
	}
	if rt.eventStream == nil && rt.hookBus == nil {
		return nil
	}
	event := events.Event{
		Kind:    kind,
		Payload: payload,
	}
	event = events.SanitizeEvent(event)
	if rt.hookBus != nil {
		var err error
		event, err = rt.hookBus.Emit(ctx, event)
		if err != nil {
			return err
		}
		event = events.SanitizeEvent(event)
	}
	if rt.eventStream == nil {
		if rt.stateStore == nil {
			return nil
		}
		_, err := rt.stateStore.AddEvent(ctx, event)
		return err
	}
	appended, err := rt.eventStream.Append(ctx, event)
	if err != nil {
		return err
	}
	if rt.stateStore == nil {
		return nil
	}
	_, err = rt.stateStore.AddEvent(ctx, appended)
	return err
}

func (rt httpRuntime) emitSessionEvent(ctx context.Context, session runtimeplan.Session, kind events.EventKind, payload map[string]any) error {
	if rt.eventStream == nil && rt.hookBus == nil {
		return nil
	}
	event := events.Event{
		Kind:      kind,
		ExecID:    session.ExecID,
		SessionID: session.SessionID,
		TraceID:   session.TraceID,
		Payload:   payload,
	}
	event = events.SanitizeEvent(event)
	if rt.hookBus != nil {
		var err error
		event, err = rt.hookBus.Emit(ctx, event)
		if err != nil {
			return err
		}
		event = events.SanitizeEvent(event)
	}
	if rt.eventStream == nil {
		if rt.stateStore == nil {
			return nil
		}
		_, err := rt.stateStore.AddEvent(ctx, event)
		return err
	}
	appended, err := rt.eventStream.Append(ctx, event)
	if err != nil {
		return err
	}
	if rt.stateStore == nil {
		return nil
	}
	_, err = rt.stateStore.AddEvent(ctx, appended)
	return err
}

func (rt httpRuntime) emitPipelineEvent(ctx context.Context, result planRunResult, plan runtimeplan.Plan, kind events.EventKind, status string, eventErr error) error {
	payload := map[string]any{
		"method":   plan.Trigger.Method,
		"path":     plan.Trigger.Path,
		"steps":    len(plan.Steps),
		"terminal": string(plan.Terminal.Kind),
		"status":   status,
	}
	if eventErr != nil {
		payload["error"] = eventErr.Error()
	}
	return rt.emitRuntimeEvent(ctx, result, kind, payload)
}

func applyPushTerminal(ctx context.Context, terminal runtimeplan.Terminal, output string) error {
	if terminal.PushWebhookURL == "" {
		return nil
	}
	return postWebhook(ctx, terminal.PushWebhookURL, output)
}

func postWebhook(ctx context.Context, url, output string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(output))
	if err != nil {
		return err
	}
	if json.Valid([]byte(output)) {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("webhook push returned status %d", resp.StatusCode)
	}
	return nil
}

func (rt httpRuntime) applySinkTerminal(ctx context.Context, terminal runtimeplan.Terminal, result planRunResult, payloadKey string) error {
	output := result.Output
	if terminal.SinkFilePath == "" {
		if terminal.SinkLog {
			return rt.appendLogSinkEvent(ctx, result, payloadKey, output)
		}
		return nil
	}
	return writeFileSink(terminal.SinkFilePath, output)
}

func (rt httpRuntime) appendLogSinkEvent(ctx context.Context, result planRunResult, payloadKey, output string) error {
	return rt.emitRuntimeEvent(ctx, result, events.EventSinkLogged, map[string]any{
		"target":   "log",
		payloadKey: terminalLogPayload(output),
	})
}

func terminalLogPayload(output string) any {
	var decoded any
	if err := json.Unmarshal([]byte(output), &decoded); err == nil {
		if containsSensitiveJSONValue(decoded) {
			redacted, err := json.Marshal(decoded)
			if err == nil {
				return string(redacted)
			}
		}
	}
	return output
}

func containsSensitiveJSONValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		found := false
		for key, child := range typed {
			if isSensitiveLogKey(key) {
				typed[key] = "[REDACTED]"
				found = true
				continue
			}
			if containsSensitiveJSONValue(child) {
				found = true
			}
		}
		return found
	case []any:
		found := false
		for _, child := range typed {
			if containsSensitiveJSONValue(child) {
				found = true
			}
		}
		return found
	}
	return false
}

func isSensitiveLogKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "authorization", "token", "api_key", "password", "secret", "cookie":
		return true
	}
	return strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_secret") ||
		strings.HasSuffix(normalized, "_password") ||
		strings.Contains(normalized, "api_key") ||
		strings.HasSuffix(normalized, "_cookie")
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
