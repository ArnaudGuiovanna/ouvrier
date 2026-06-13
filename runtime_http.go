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

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/schema"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

const shutdownTimeout = 5 * time.Second
const maxHTTPRequestBodyBytes = 1 << 20
const maxAdminRequestBodyBytes = 1 << 20
const directReplyOKOutput = `{"status":"ok"}`

// limitAdminBody caps an admin request body to maxAdminRequestBodyBytes,
// mirroring the hardening already applied to the public trigger path
// (readHTTPRequestInput) so an admin handler cannot be made to buffer an
// unbounded body. Each handler keeps its own decode and error contract.
func limitAdminBody(w http.ResponseWriter, req *http.Request) {
	if req.Body == nil {
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxAdminRequestBodyBytes)
}

var checkBashIsolationAvailable = tools.CheckBashIsolationAvailable

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
	return newHTTPHandlerFromRoutes(routes, runtime)
}

func newWebhookHandlerWithRuntime(nodes []Node, runtime httpRuntime) (http.Handler, error) {
	routes, err := webhookRoutesFromNodes(nodes)
	if err != nil {
		return nil, err
	}
	return newHTTPHandlerFromRoutes(routes, runtime)
}

func newHTTPCompatibleHandlerWithRuntime(nodes []Node, runtime httpRuntime) (http.Handler, error) {
	routes, err := httpCompatibleRoutesFromNodes(nodes)
	if err != nil {
		return nil, err
	}
	return newHTTPHandlerFromRoutes(routes, runtime)
}

func newHTTPHandlerFromRoutes(routes []httpRoute, runtime httpRuntime) (http.Handler, error) {
	plans := make([]runtimeplan.Plan, 0, len(routes))
	for _, route := range routes {
		plans = append(plans, route.plan)
	}
	return newHTTPHandlerFromRoutesAndPlans(routes, plans, runtime)
}

func newHTTPHandlerFromRoutesAndPlans(routes []httpRoute, plans []runtimeplan.Plan, runtime httpRuntime) (http.Handler, error) {
	runtime, err := prepareHTTPServeRuntime(routes, plans, runtime)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	registerHTTPAdminRoutes(mux, runtime)
	if err := registerPublicHTTPRoutes(mux, routes, runtime); err != nil {
		return nil, err
	}
	return newRuntimeHTTPHandler(mux, runtime.async), nil
}

// prepareHTTPServeRuntime applies the serve-time runtime setup shared by the
// combined (v0.2 shared-port) and split (OUVRIER_ADMIN_ADDR) handler layouts:
// trigger security validation, runtime guards, async group, admin route/plan
// wiring, and durable-run recovery.
func prepareHTTPServeRuntime(routes []httpRoute, plans []runtimeplan.Plan, runtime httpRuntime) (httpRuntime, error) {
	if err := validateHTTPTriggerSecurityConfig(routes, runtime); err != nil {
		return httpRuntime{}, err
	}
	if err := validateRuntimeGuardsForPlans(plans); err != nil {
		return httpRuntime{}, err
	}
	runtime = runtime.withAsyncGroup()
	runtime.adminRoutes = routes
	runtime.adminPlans = adminPlanRoutesFromPlans(plans)
	startDurableRunRecovery(runtime, plans)
	return runtime, nil
}

func registerPublicHTTPRoutes(mux *http.ServeMux, routes []httpRoute, runtime httpRuntime) error {
	for _, route := range routes {
		route.runtime = runtime
		if err := registerHTTPRoute(mux, route); err != nil {
			return err
		}
	}
	return nil
}

// newSplitHTTPHandlersFromRoutesAndPlans builds the OUVRIER_ADMIN_ADDR layout:
// a public handler carrying only the trigger routes, and an admin handler
// carrying the full registerHTTPAdminRoutes surface (every /admin/* route,
// the dev-mode /dev viewer, and /metrics) while 404ing everything else. Both
// handlers share one runtime — state store, event stream, async group — so
// admin endpoints on the second listener observe triggers fired on the first
// and graceful shutdown drains both together. With OUVRIER_METRICS_PUBLIC,
// /metrics is additionally registered on the public handler (same bearer
// auth) for Prometheus scrapers that cannot reach the loopback admin port.
func newSplitHTTPHandlersFromRoutesAndPlans(routes []httpRoute, plans []runtimeplan.Plan, runtime httpRuntime) (publicHandler, adminHandler http.Handler, err error) {
	runtime, err = prepareHTTPServeRuntime(routes, plans, runtime)
	if err != nil {
		return nil, nil, err
	}

	publicMux := http.NewServeMux()
	// Register the opt-in public /metrics before trigger routes, mirroring
	// the combined layout's admin-first order: a user trigger declared at
	// GET /metrics then fails route registration with ErrInvalidNode
	// instead of panicking the mux.
	if metricsPublicOptIn() {
		publicMux.HandleFunc("GET /metrics", runtime.serveMetrics)
	}
	if err := registerPublicHTTPRoutes(publicMux, routes, runtime); err != nil {
		return nil, nil, err
	}
	adminMux := http.NewServeMux()
	registerHTTPAdminRoutes(adminMux, runtime)
	return newRuntimeHTTPHandler(publicMux, runtime.async), newRuntimeHTTPHandler(adminMux, runtime.async), nil
}

// newSplitHTTPCompatibleHandlersWithRuntime is the split-listener counterpart
// of newHTTPCompatibleHandlerWithRuntime, used by Run when OUVRIER_ADMIN_ADDR
// is set.
func newSplitHTTPCompatibleHandlersWithRuntime(nodes []Node, runtime httpRuntime) (publicHandler, adminHandler http.Handler, err error) {
	routes, err := httpCompatibleRoutesFromNodes(nodes)
	if err != nil {
		return nil, nil, err
	}
	plans := make([]runtimeplan.Plan, 0, len(routes))
	for _, route := range routes {
		plans = append(plans, route.plan)
	}
	return newSplitHTTPHandlersFromRoutesAndPlans(routes, plans, runtime)
}

func newAdminHandlerWithRuntime(plans []runtimeplan.Plan, runtime httpRuntime) (http.Handler, error) {
	if err := validateRuntimeGuardsForPlans(plans); err != nil {
		return nil, err
	}
	runtime = runtime.withAsyncGroup()
	runtime.adminPlans = adminPlanRoutesFromPlans(plans)
	startDurableRunRecovery(runtime, plans)
	mux := http.NewServeMux()
	registerHTTPAdminRoutes(mux, runtime)
	return newRuntimeHTTPHandler(mux, runtime.async), nil
}

func validateRuntimeGuardsForPlans(plans []runtimeplan.Plan) error {
	for _, plan := range plans {
		if err := validateRuntimeGuardsForPipeline(runtimeplan.Pipeline{Steps: plan.Steps}); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeGuardsForPipeline(pipeline runtimeplan.Pipeline) error {
	for _, step := range pipeline.Steps {
		if err := validateRuntimeGuardsForStep(step); err != nil {
			return err
		}
	}
	return nil
}

func validateRuntimeGuardsForStep(step runtimeplan.Step) error {
	for _, bash := range step.Bash {
		if !bash.UnsafeHostExecution {
			name := strings.TrimSpace(bash.Name)
			if name == "" {
				name = defaultBashToolName
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := checkBashIsolationAvailable(ctx)
			cancel()
			if err != nil {
				return fmt.Errorf("%w: isolated Bash sandbox unavailable for tool %q: %w", ErrInvalidNode, name, err)
			}
		}
	}
	for _, branch := range step.Branches {
		if err := validateRuntimeGuardsForPipeline(branch); err != nil {
			return err
		}
	}
	if err := validateRuntimeGuardsForPipeline(step.MapPipeline); err != nil {
		return err
	}
	for _, subAgent := range step.SubAgents {
		if err := validateRuntimeGuardsForPipeline(subAgent.Pipeline); err != nil {
			return err
		}
	}
	return nil
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
	input, session, ok := r.prepareRequestInput(w, req)
	if !ok {
		return
	}
	if !r.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	defer r.releaseWorker()

	result, err := r.runtime.startDirectPlanExecution(req.Context(), r.plan, input, session)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}

	switch r.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		if r.plan.Terminal.Async {
			if err := r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, nil); err != nil {
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
			writeJSONStatus(w, http.StatusAccepted, "accepted")
			return
		}
		result.Output = directReplyOKOutput
		if err := r.runtime.validateObservedTerminalReplyOutput(req.Context(), r.plan, result); err != nil {
			_ = r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, err)
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		if err := r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, nil); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		if r.plan.Terminal.SSE {
			writeSSEOutput(w, http.StatusOK, "output", directReplyOKOutput)
			return
		}
		writeJSONStatus(w, http.StatusOK, "ok")
	case runtimeplan.TerminalPush:
		if r.plan.Terminal.PushWebhookURL != "" || r.plan.Terminal.PushQueueURI != "" {
			if err := r.runtime.applyPushTerminal(req.Context(), r.plan.Terminal, result, input); err != nil {
				_ = r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, err)
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
		}
		if err := r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, nil); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	case runtimeplan.TerminalSink:
		if r.plan.Terminal.SinkFilePath != "" || r.plan.Terminal.SinkLog {
			if err := r.runtime.applySinkTerminal(req.Context(), r.plan.Terminal, result, "input"); err != nil {
				_ = r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, err)
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
		}
		if err := r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, nil); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	default:
		_ = r.runtime.finishDirectPlanExecution(req.Context(), r.plan, result, errors.New("terminal missing"))
		writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
	}
}

func (rt httpRuntime) startDirectPlanExecution(ctx context.Context, plan runtimeplan.Plan, input string, session *runtimeplan.Session) (planRunResult, error) {
	pipelineSession, err := pipelineSessionForPlan(plan, session)
	if err != nil {
		return planRunResult{Output: input}, err
	}
	result := planRunResult{Output: input, Session: pipelineSession, HasSession: true}
	if err := rt.startPipelineExecution(ctx, pipelineSession, plan); err != nil {
		return result, err
	}
	return result, nil
}

func (rt httpRuntime) finishDirectPlanExecution(ctx context.Context, plan runtimeplan.Plan, result planRunResult, terminalErr error) error {
	if !result.HasSession {
		return nil
	}
	status := "completed"
	if terminalErr != nil {
		status = "failed"
	}
	return rt.finishPipelineExecution(ctx, result.Session, plan, status, terminalErr)
}

func (r httpRoute) servePipeline(w http.ResponseWriter, req *http.Request) {
	eventStartID := uint64(0)
	if r.plan.Terminal.Kind == runtimeplan.TerminalReply && r.plan.Terminal.SSE {
		r.runtime = r.runtime.withEventStream()
		eventStartID = r.runtime.lastEventID()
	}
	input, session, ok := r.prepareRequestInput(w, req)
	if !ok {
		return
	}

	if r.plan.Terminal.Async {
		if !r.tryAcquireWorker() {
			writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
			return
		}
		if !r.runtime.startAsync(func(ctx context.Context) {
			defer r.releaseWorker()
			_, _ = r.runtime.runPlanWithSession(ctx, r.plan, input, session)
		}) {
			r.releaseWorker()
			writeJSONStatus(w, http.StatusServiceUnavailable, "shutting_down")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
		return
	}
	if r.plan.Terminal.Kind == runtimeplan.TerminalReply && r.plan.Terminal.SSE {
		r.servePipelineSSE(w, req, input, session, eventStartID)
		return
	}

	if !r.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	defer r.releaseWorker()

	result, err := r.runtime.runPlanResultWithSession(req.Context(), r.plan, input, session)
	if err != nil {
		if suspended, ok := suspendedExecutionError(err); ok {
			writeSuspendedResponse(w, suspended)
			return
		}
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
		if err := r.runtime.applyPushTerminal(req.Context(), r.plan.Terminal, result, output); err != nil {
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

func validateHTTPTriggerSecurityConfig(routes []httpRoute, rt httpRuntime) error {
	for _, route := range routes {
		if strings.TrimSpace(route.plan.Trigger.IdempotencyHeader) != "" && rt.stateStore == nil {
			return fmt.Errorf("%w: IdempotencyKey requires a StateStore", ErrInvalidNode)
		}
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

func (r httpRoute) prepareRequestInput(w http.ResponseWriter, req *http.Request) (string, *runtimeplan.Session, bool) {
	body, err := readHTTPRequestInput(req)
	if err != nil {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return "", nil, false
	}
	if !r.verifyRequestSignature(w, req, []byte(body)) {
		return "", nil, false
	}
	session, duplicate, ok := r.runtime.reserveTriggerIdempotency(w, req, r.plan)
	if !ok {
		return "", nil, false
	}
	if duplicate {
		return "", session, false
	}
	input, err := r.buildPipelineInput(body, httpPathParams(req, r.plan.Trigger.Path))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_request")
		return "", nil, false
	}
	return input, session, true
}

func (r httpRoute) verifyRequestSignature(w http.ResponseWriter, req *http.Request, payload []byte) bool {
	env := strings.TrimSpace(r.plan.Trigger.SignatureEnv)
	headerName := strings.TrimSpace(r.plan.Trigger.SignatureHeader)
	if env == "" && headerName == "" {
		return true
	}
	headerValue := strings.TrimSpace(req.Header.Get(headerName))
	if headerValue == "" {
		if err := r.emitSignatureDecision(req.Context(), "missing"); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "event_stream_error")
			return false
		}
		writeJSONStatus(w, http.StatusUnauthorized, "signature_missing")
		return false
	}
	secret := os.Getenv(env)
	if secret == "" {
		if err := r.emitSignatureDecision(req.Context(), "secret_missing"); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "event_stream_error")
			return false
		}
		writeJSONStatus(w, http.StatusInternalServerError, "signature_secret_missing")
		return false
	}
	if !validHMACSHA256Signature(secret, payload, headerValue) {
		if err := r.emitSignatureDecision(req.Context(), "invalid"); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "event_stream_error")
			return false
		}
		writeJSONStatus(w, http.StatusForbidden, "signature_invalid")
		return false
	}
	if err := r.emitSignatureDecision(req.Context(), "valid"); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "event_stream_error")
		return false
	}
	return true
}

func (r httpRoute) emitSignatureDecision(ctx context.Context, decision string) error {
	return r.runtime.emitRuntimeEvent(ctx, planRunResult{}, events.EventSignatureDecision, map[string]any{
		"scope":    "trigger",
		"method":   r.plan.Trigger.Method,
		"path":     r.plan.Trigger.Path,
		"header":   http.CanonicalHeaderKey(r.plan.Trigger.SignatureHeader),
		"decision": decision,
	})
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

func (rt httpRuntime) reserveTriggerIdempotency(w http.ResponseWriter, req *http.Request, plan runtimeplan.Plan) (*runtimeplan.Session, bool, bool) {
	headerName := strings.TrimSpace(plan.Trigger.IdempotencyHeader)
	if headerName == "" {
		return nil, false, true
	}
	value := strings.TrimSpace(req.Header.Get(headerName))
	if value == "" {
		writeJSONStatus(w, http.StatusBadRequest, "idempotency_key_missing")
		return nil, false, false
	}
	if rt.stateStore == nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_missing")
		return nil, false, false
	}
	session, err := newHTTPPipelineSession(plan)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "idempotency_error")
		return nil, false, false
	}

	key := triggerIdempotencyReservationKey(plan, headerName, value)
	existingExecID, reserved, err := rt.stateStore.ReserveIdempotency(req.Context(), key, session.ExecID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return nil, false, false
	}
	payload := map[string]any{
		"scope":  "trigger",
		"method": plan.Trigger.Method,
		"path":   plan.Trigger.Path,
		"header": http.CanonicalHeaderKey(headerName),
	}
	if reserved {
		payload["decision"] = "reserved"
		if err := rt.emitSessionEvent(req.Context(), session, events.EventIdempotencyDecision, payload); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "event_stream_error")
			return nil, false, false
		}
		return &session, false, true
	}
	payload["decision"] = "duplicate"
	payload["existing_exec_id"] = existingExecID
	if err := rt.emitRuntimeEvent(req.Context(), planRunResult{}, events.EventIdempotencyDecision, payload); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "event_stream_error")
		return nil, false, false
	}
	writeJSONStatus(w, http.StatusAccepted, "duplicate_idempotency_key")
	return &session, true, true
}

func triggerIdempotencyReservationKey(plan runtimeplan.Plan, headerName, value string) string {
	sum := sha256.Sum256([]byte(value))
	return strings.Join([]string{
		"trigger",
		plan.Trigger.Method,
		plan.Trigger.Path,
		http.CanonicalHeaderKey(headerName),
		hex.EncodeToString(sum[:]),
	}, ":")
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

func (r httpRoute) buildPipelineInput(body string, pathParams map[string]string) (string, error) {
	switch r.plan.Trigger.Kind {
	case runtimeplan.TriggerWebhook:
		return buildWebhookPipelineInput(body, r.plan.Trigger.Value)
	default:
		return buildHTTPPipelineInput(body, pathParams)
	}
}

func buildWebhookPipelineInput(body, provider string) (string, error) {
	input := map[string]any{
		"trigger":  "webhook",
		"provider": strings.TrimSpace(provider),
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
	if lastStep.Kind != runtimeplan.StepPipe {
		return false
	}
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
		blocked := event
		updated, err := rt.hookBus.Emit(ctx, event)
		if err != nil {
			return errors.Join(err, rt.emitHookFailureEvent(ctx, blocked, err))
		}
		event = updated
		event = events.SanitizeEvent(event)
	}
	return rt.appendRuntimeEvent(ctx, event)
}

func (rt httpRuntime) appendRuntimeEvent(ctx context.Context, event events.Event) error {
	event = events.SanitizeEvent(event)
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
		blocked := event
		updated, err := rt.hookBus.Emit(ctx, event)
		if err != nil {
			return errors.Join(err, rt.emitHookFailureEvent(ctx, blocked, err))
		}
		event = updated
		event = events.SanitizeEvent(event)
	}
	return rt.appendRuntimeEvent(ctx, event)
}

func (rt httpRuntime) emitHookFailureEvent(ctx context.Context, blocked events.Event, err error) error {
	if err == nil || (rt.eventStream == nil && rt.stateStore == nil) {
		return nil
	}
	event := events.Event{
		Kind:      events.EventHookFailed,
		ExecID:    blocked.ExecID,
		SessionID: blocked.SessionID,
		TraceID:   blocked.TraceID,
		Payload: map[string]any{
			"blocked_kind": string(events.CanonicalKind(blocked.Kind)),
			"error":        err.Error(),
		},
	}
	return rt.appendRuntimeEvent(ctx, event)
}

func (rt httpRuntime) emitPipelineEvent(ctx context.Context, result planRunResult, plan runtimeplan.Plan, kind events.EventKind, status string, eventErr error) error {
	payload := map[string]any{
		"method":   plan.Trigger.Method,
		"path":     plan.Trigger.Path,
		"steps":    len(plan.Steps),
		"terminal": string(plan.Terminal.Kind),
		"status":   status,
	}
	if rt.cronLease != nil {
		payload["lease"] = rt.cronLease.name
		payload["holder"] = rt.cronLease.holder
		payload["fence"] = rt.cronLease.fence
	}
	if eventErr != nil {
		payload["error"] = eventErr.Error()
	}
	return rt.emitRuntimeEvent(ctx, result, kind, payload)
}

func (rt httpRuntime) applyPushTerminal(ctx context.Context, terminal runtimeplan.Terminal, result planRunResult, output string) error {
	if terminal.PushWebhookURL != "" {
		return rt.executeOutputTool(ctx, result, "ouvrier_push_webhook", tools.Metadata{
			ActionKind:  policy.ActionPushWebhook,
			Kind:        tools.ToolKindOutput,
			Target:      terminal.PushWebhookURL,
			Effect:      policy.EffectSideEffecting,
			SideEffects: []string{"webhook"},
			InputSchema: outputToolInputSchema(),
		}, outputToolHandlerFunc(func(ctx context.Context, output string) error {
			return postWebhook(ctx, terminal.PushWebhookURL, output)
		}), output)
	}
	if terminal.PushQueueURI != "" {
		return rt.executeOutputTool(ctx, result, "ouvrier_push_queue", tools.Metadata{
			ActionKind:  policy.ActionPushQueue,
			Kind:        tools.ToolKindOutput,
			Target:      terminal.PushQueueURI,
			Effect:      policy.EffectSideEffecting,
			SideEffects: []string{"queue"},
			InputSchema: outputToolInputSchema(),
		}, outputToolHandlerFunc(func(ctx context.Context, output string) error {
			return publishQueue(ctx, terminal.PushQueueURI, output)
		}), output)
	}
	return nil
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
			return rt.executeOutputTool(ctx, result, "ouvrier_sink_log", tools.Metadata{
				ActionKind:  policy.ActionSinkLog,
				Kind:        tools.ToolKindOutput,
				Effect:      policy.EffectReadOnly,
				InputSchema: outputToolInputSchema(),
			}, outputToolHandlerFunc(func(ctx context.Context, output string) error {
				return rt.appendLogSinkEvent(ctx, result, payloadKey, output)
			}), output)
		}
		return nil
	}
	return rt.executeOutputTool(ctx, result, "ouvrier_sink_file", tools.Metadata{
		ActionKind:  policy.ActionSinkFile,
		Kind:        tools.ToolKindOutput,
		Target:      terminal.SinkFilePath,
		Effect:      policy.EffectSideEffecting,
		SideEffects: []string{"file"},
		InputSchema: outputToolInputSchema(),
	}, outputToolHandlerFunc(func(ctx context.Context, output string) error {
		resolvedPath, err := rt.resolveFileSinkPath(terminal.SinkFilePath)
		if err != nil {
			return err
		}
		return writeFileSink(resolvedPath, output)
	}), output)
}

func (rt httpRuntime) resolveFileSinkPath(path string) (string, error) {
	if rt.sandbox == nil {
		return "", errors.New("file sink requires sandbox")
	}
	return rt.sandbox.Resolve(path)
}

func (rt httpRuntime) appendLogSinkEvent(ctx context.Context, result planRunResult, payloadKey, output string) error {
	return rt.emitRuntimeEvent(ctx, result, events.EventSinkLogged, map[string]any{
		"target":   "log",
		payloadKey: terminalLogPayload(output),
	})
}

func terminalLogPayload(output string) any {
	return events.RedactJSONText(output)
}

func writeFileSink(path, output string) error {
	return os.WriteFile(path, []byte(output), 0o644)
}

type outputToolArgs struct {
	Output string `json:"output"`
}

type outputToolHandlerFunc func(context.Context, string) error

func (f outputToolHandlerFunc) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	var args outputToolArgs
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return provider.ToolResult{}, err
	}
	if err := f(ctx, args.Output); err != nil {
		return provider.ToolResult{}, err
	}
	return provider.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    []byte(`"ok"`),
	}, nil
}

func outputToolInputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"output":{"type":"string"}},"required":["output"],"additionalProperties":false}`)
}

func (rt httpRuntime) executeOutputTool(ctx context.Context, result planRunResult, name string, metadata tools.Metadata, handler tools.Handler, output string) error {
	executor := rt.toolExecutor
	if executor == nil {
		executor = tools.NewExecutor()
	}
	scoped := executor.NewScope()
	if err := scoped.RegisterHandler(name, handler, tools.WithMetadata(metadata)); err != nil {
		return err
	}
	args, err := json.Marshal(outputToolArgs{Output: output})
	if err != nil {
		return err
	}
	call := provider.ToolCall{
		ID:        name,
		Name:      name,
		Arguments: args,
	}
	if err := rt.emitOutputToolCallEvent(ctx, result, events.EventToolCallStarted, call, metadata, nil); err != nil {
		return err
	}
	toolCtx := tools.ContextWithPermissionDecisionObserver(ctx, func(ctx context.Context, audit tools.PermissionDecisionAudit) error {
		return rt.emitOutputToolPermissionDecision(ctx, result, audit)
	})
	toolResult, err := scoped.Execute(toolCtx, call)
	if err != nil {
		emitErr := rt.emitOutputToolCallEvent(ctx, result, events.EventToolCallFailed, call, metadata, err)
		return errors.Join(err, emitErr)
	}
	if toolResult.IsError {
		toolErr := fmt.Errorf("output tool %s failed: %s", name, outputToolErrorText(toolResult.Content))
		emitErr := rt.emitOutputToolCallEvent(ctx, result, events.EventToolCallFailed, call, metadata, toolErr)
		return errors.Join(toolErr, emitErr)
	}
	if err := rt.emitOutputToolCallEvent(ctx, result, events.EventToolCallCompleted, call, metadata, nil); err != nil {
		return err
	}
	return nil
}

func (rt httpRuntime) emitOutputToolCallEvent(ctx context.Context, result planRunResult, kind events.EventKind, call provider.ToolCall, metadata tools.Metadata, eventErr error) error {
	payload := map[string]any{
		"tool":          call.Name,
		"tool_call_id":  call.ID,
		"output_action": true,
	}
	if metadata.ActionKind != "" {
		payload["action"] = string(metadata.ActionKind)
	}
	if metadata.Kind != "" {
		payload["tool_kind"] = string(metadata.Kind)
	}
	if metadata.Effect != "" {
		payload["effect"] = string(metadata.Effect)
	}
	if eventErr != nil {
		payload["error"] = eventErr.Error()
	}
	return rt.emitRuntimeEvent(ctx, result, kind, payload)
}

func (rt httpRuntime) emitOutputToolPermissionDecision(ctx context.Context, result planRunResult, audit tools.PermissionDecisionAudit) error {
	payload := map[string]any{
		"action":        string(audit.Action.Kind),
		"tool":          audit.Action.ToolName,
		"tool_kind":     audit.Action.ToolKind,
		"allowed":       audit.Decision.Allowed && audit.Err == nil,
		"effect":        string(audit.Action.Effect),
		"output_action": true,
	}
	if len(audit.Action.SideEffects) > 0 {
		payload["side_effects"] = append([]string(nil), audit.Action.SideEffects...)
	}
	if target := strings.TrimSpace(audit.Action.Target); target != "" {
		sum := sha256.Sum256([]byte(target))
		payload["target_hash"] = hex.EncodeToString(sum[:])
	}
	if audit.Err != nil {
		payload["error"] = audit.Err.Error()
	} else if !audit.Decision.Allowed && audit.Decision.Reason != "" {
		payload["reason"] = audit.Decision.Reason
	}
	return rt.emitRuntimeEvent(ctx, result, events.EventPermissionDecision, payload)
}

func outputToolErrorText(content json.RawMessage) string {
	var text string
	if err := json.Unmarshal(content, &text); err == nil && strings.TrimSpace(text) != "" {
		return text
	}
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return "tool returned error result"
	}
	return string(content)
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serveHTTPWithContext(ctx, addr, handler)
}

// serveSplitHTTP serves the public handler on addr and the admin handler on
// adminAddr (OUVRIER_ADMIN_ADDR) under one signal-driven shutdown. Both
// listeners mirror the single-listener lifecycle: a failure on either one
// cancels the other (which then shuts down gracefully) and fails Run the same
// way a single listener failure does.
func serveSplitHTTP(addr string, publicHandler http.Handler, adminAddr string, adminHandler http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serveSplitHTTPWithContext(ctx, addr, publicHandler, adminAddr, adminHandler)
}

func serveSplitHTTPWithContext(ctx context.Context, addr string, publicHandler http.Handler, adminAddr string, adminHandler http.Handler) error {
	return runSupervisedRuntimes(ctx,
		func(ctx context.Context) error {
			return serveHTTPWithContext(ctx, addr, publicHandler)
		},
		func(ctx context.Context) error {
			return serveHTTPWithContext(ctx, adminAddr, adminHandler)
		},
	)
}

// serveAdminOnlyHTTPWithContext serves the HTTP surface of a worker whose
// only HTTP routes are the admin ones (cron and stream runtimes). When
// OUVRIER_ADMIN_ADDR is unset the admin handler binds the public addr exactly
// as in v0.2. When set, the admin surface moves to the dedicated listener and
// the public addr still binds — keeping Run's contract of owning addr — but
// answers 404 for everything, so admin routes are never network reachable.
func serveAdminOnlyHTTPWithContext(ctx context.Context, addr string, adminHandler http.Handler) error {
	adminAddr := adminAddrFromEnv()
	if adminAddr == "" {
		return serveHTTPWithContext(ctx, addr, adminHandler)
	}
	return serveSplitHTTPWithContext(ctx, addr, http.NewServeMux(), adminAddr, adminHandler)
}

func serveHTTPWithContext(ctx context.Context, addr string, handler http.Handler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}

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
			if shutdownable, ok := handler.(interface{ Shutdown(context.Context) error }); ok {
				_ = shutdownable.Shutdown(shutdownCtx)
			}
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		var shutdownErr error
		if shutdownable, ok := handler.(interface{ Shutdown(context.Context) error }); ok {
			shutdownErr = shutdownable.Shutdown(shutdownCtx)
		}
		return errors.Join(<-serveErr, shutdownErr)
	}
}
