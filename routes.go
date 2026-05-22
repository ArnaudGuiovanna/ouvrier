package ovr

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"ouvrier/internal/events"
	runtimeplan "ouvrier/internal/runtime"
	"ouvrier/internal/state"
)

const defaultAdminTraceLimit = 20
const maxAdminTraceLimit = 200

type httpRoute struct {
	method     string
	path       string
	plan       runtimeplan.Plan
	runtime    httpRuntime
	workerPool chan struct{}
}

type adminPlanRoute struct {
	plan       runtimeplan.Plan
	workerPool chan struct{}
}

func httpRoutesFromNodes(nodes []Node) ([]httpRoute, error) {
	plans, err := compilePlans(nodes)
	if err != nil {
		return nil, err
	}

	routes := make([]httpRoute, 0, len(plans))
	for _, plan := range plans {
		if plan.Trigger.Kind != runtimeplan.TriggerHTTP {
			return nil, fmt.Errorf("%w: only HTTP triggers are supported by this runtime slice", ErrRunNotImplemented)
		}
		routes = append(routes, httpRouteFromHTTPPlan(plan))
	}
	return routes, nil
}

func httpCompatibleRoutesFromNodes(nodes []Node) ([]httpRoute, error) {
	plans, err := compilePlans(nodes)
	if err != nil {
		return nil, err
	}

	routes := make([]httpRoute, 0, len(plans))
	for _, plan := range plans {
		switch plan.Trigger.Kind {
		case runtimeplan.TriggerHTTP:
			routes = append(routes, httpRouteFromHTTPPlan(plan))
		case runtimeplan.TriggerWebhook:
			route, err := httpRouteFromWebhookPlan(plan)
			if err != nil {
				return nil, err
			}
			routes = append(routes, route)
		default:
			return nil, fmt.Errorf("%w: only HTTP-compatible triggers are supported by this runtime slice", ErrRunNotImplemented)
		}
	}
	return routes, nil
}

func webhookRoutesFromNodes(nodes []Node) ([]httpRoute, error) {
	plans, err := compilePlans(nodes)
	if err != nil {
		return nil, err
	}

	routes := make([]httpRoute, 0, len(plans))
	for _, plan := range plans {
		if plan.Trigger.Kind != runtimeplan.TriggerWebhook {
			return nil, fmt.Errorf("%w: only webhook triggers are supported by this runtime slice", ErrRunNotImplemented)
		}
		route, err := httpRouteFromWebhookPlan(plan)
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func httpRouteFromHTTPPlan(plan runtimeplan.Plan) httpRoute {
	return httpRoute{
		method:     plan.Trigger.Method,
		path:       plan.Trigger.Path,
		plan:       plan,
		workerPool: newWorkerPool(plan.Trigger.WorkerPool),
	}
}

func httpRouteFromWebhookPlan(plan runtimeplan.Plan) (httpRoute, error) {
	path, err := webhookRoutePath(plan.Trigger.Value)
	if err != nil {
		return httpRoute{}, err
	}
	plan.Trigger.Method = http.MethodPost
	plan.Trigger.Path = path
	return httpRoute{
		method:     http.MethodPost,
		path:       path,
		plan:       plan,
		workerPool: newWorkerPool(plan.Trigger.WorkerPool),
	}, nil
}

func webhookRoutePath(provider string) (string, error) {
	provider = strings.TrimSpace(provider)
	if !validWebhookProviderName(provider) {
		return "", fmt.Errorf("%w: webhook provider must contain only letters, digits, dot, dash, or underscore", ErrInvalidNode)
	}
	return "/webhooks/" + provider, nil
}

func newWorkerPool(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

func adminPlanRoutesFromPlans(plans []runtimeplan.Plan) []adminPlanRoute {
	routes := make([]adminPlanRoute, 0, len(plans))
	for _, plan := range plans {
		routes = append(routes, adminPlanRoute{
			plan:       plan,
			workerPool: newWorkerPool(plan.Trigger.WorkerPool),
		})
	}
	return routes
}

func registerHTTPAdminRoutes(mux *http.ServeMux, rt httpRuntime) {
	mux.HandleFunc("GET /admin/health", rt.serveAdminHealth)
	mux.HandleFunc("GET /admin/status", rt.serveAdminStatus)
	mux.HandleFunc("GET /admin/traces", rt.serveAdminTraces)
	mux.HandleFunc("GET /admin/traces/{execID}", rt.serveAdminTrace)
	mux.HandleFunc("POST /admin/trigger", rt.serveAdminTrigger)
}

func (rt httpRuntime) serveAdminHealth(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	writeJSON(w, http.StatusOK, adminHealthResponse{
		Status:      "ok",
		StateStore:  rt.stateStore != nil,
		EventStream: rt.eventStream != nil,
	})
}

func (rt httpRuntime) serveAdminStatus(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}

	response := adminStatusResponse{
		Status:   "ok",
		ByStatus: map[string]int{},
	}
	recorded, err := rt.adminEvents(req.Context(), "")
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.Events = len(recorded)
	response.SchemaValidationPassed = countAdminEventsByKind(recorded, events.EventSchemaValidationPassed)
	response.SchemaValidationFailed = countAdminEventsByKind(recorded, events.EventSchemaValidationFailed)
	response.SchemaRepairsStarted = countAdminEventsByKind(recorded, events.EventSchemaRepairStarted)
	response.SchemaRepairsCompleted = countAdminEventsByKind(recorded, events.EventSchemaRepairCompleted)
	response.SchemaViolations = response.SchemaValidationFailed
	llm := summarizeAdminLLMUsage(recorded)
	response.LLMCalls = llm.Calls
	response.InputTokens = llm.InputTokens
	response.OutputTokens = llm.OutputTokens
	response.CostUSD = llm.CostUSD
	response.AverageLatencyMS = llm.AverageLatencyMS()
	if rt.stateStore == nil {
		writeJSON(w, http.StatusOK, response)
		return
	}

	sessions, err := rt.stateStore.Sessions(req.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.Sessions = len(sessions)

	executions, err := rt.stateStore.Executions(req.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.Executions = len(executions)
	for _, execution := range executions {
		response.ByStatus[string(execution.Status)]++
	}
	violations, err := rt.stateStore.SchemaViolations(req.Context(), "")
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.SchemaViolations = len(violations)
	writeJSON(w, http.StatusOK, response)
}

func (rt httpRuntime) serveAdminTraces(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	recorded, err := rt.adminEvents(req.Context(), "")
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	traces := summarizeAdminTraces(recorded, parseAdminTraceLimit(req.URL.Query().Get("last")))
	writeJSON(w, http.StatusOK, adminTracesResponse{
		Status: "ok",
		Traces: traces,
	})
}

func (rt httpRuntime) serveAdminTrace(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	execID := strings.TrimSpace(req.PathValue("execID"))
	if execID == "" {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}

	response := adminTraceResponse{Status: "ok"}
	if rt.stateStore != nil {
		execution, ok, err := rt.stateStore.Execution(req.Context(), execID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		if ok {
			response.Execution = adminExecutionResponseFromState(execution)
		}

		sessions, err := rt.stateStore.Sessions(req.Context())
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		for _, session := range sessions {
			if session.ExecID == execID {
				response.Sessions++
			}
		}

		violations, err := rt.stateStore.SchemaViolations(req.Context(), execID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		response.SchemaViolations = len(violations)
	}

	recorded, err := rt.adminEvents(req.Context(), execID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	for _, event := range recorded {
		response.Events = append(response.Events, adminEventResponseFromEvent(event))
	}
	if response.Execution == nil && len(response.Events) == 0 {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}

	writeJSON(w, http.StatusOK, response)
}

func (rt httpRuntime) adminEvents(ctx context.Context, execID string) ([]events.Event, error) {
	if rt.stateStore != nil {
		recorded, err := rt.stateStore.Events(ctx, execID)
		if err != nil {
			return nil, err
		}
		if len(recorded) > 0 || rt.eventStream == nil {
			return recorded, nil
		}
	}
	if rt.eventStream == nil {
		return nil, nil
	}
	recorded := rt.eventStream.List()
	if execID == "" {
		return recorded, nil
	}
	filtered := make([]events.Event, 0, len(recorded))
	for _, event := range recorded {
		if event.ExecID == execID {
			filtered = append(filtered, event)
		}
	}
	return filtered, nil
}

func countAdminEventsByKind(recorded []events.Event, kind events.EventKind) int {
	count := 0
	for _, event := range recorded {
		if events.CanonicalKind(event.Kind) == kind {
			count++
		}
	}
	return count
}

func summarizeAdminLLMUsage(recorded []events.Event) adminLLMUsageSummary {
	var summary adminLLMUsageSummary
	for _, event := range recorded {
		summary.AddEvent(event)
	}
	return summary
}

func adminNumericPayload(payload map[string]any, key string) float64 {
	if payload == nil {
		return 0
	}
	switch value := payload[key].(type) {
	case int:
		return float64(value)
	case int8:
		return float64(value)
	case int16:
		return float64(value)
	case int32:
		return float64(value)
	case int64:
		return float64(value)
	case uint:
		return float64(value)
	case uint8:
		return float64(value)
	case uint16:
		return float64(value)
	case uint32:
		return float64(value)
	case uint64:
		return float64(value)
	case float32:
		return float64(value)
	case float64:
		return value
	default:
		return 0
	}
}

func (rt httpRuntime) serveAdminTrigger(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	var trigger adminTriggerRequest
	if err := json.NewDecoder(req.Body).Decode(&trigger); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	trigger.Method = strings.ToUpper(strings.TrimSpace(trigger.Method))
	trigger.Path = strings.TrimSpace(trigger.Path)

	switch trigger.kind() {
	case runtimeplan.TriggerHTTP, runtimeplan.TriggerWebhook:
		rt.serveAdminHTTPTrigger(w, req, trigger)
	case runtimeplan.TriggerCron:
		rt.serveAdminCronTrigger(w, req, trigger)
	case runtimeplan.TriggerStream:
		rt.serveAdminStreamTrigger(w, req, trigger)
	default:
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
	}
}

func (rt httpRuntime) serveAdminHTTPTrigger(w http.ResponseWriter, req *http.Request, trigger adminTriggerRequest) {
	if trigger.Method == "" || trigger.Path == "" {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	route, pathParams, ok := rt.adminTriggerRoute(trigger.Method, trigger.Path)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	body, err := trigger.bodyString()
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	input, err := route.buildPipelineInput(body, pathParams)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	rt.executeAdminTriggerRoute(w, req, route, input)
}

func (rt httpRuntime) serveAdminCronTrigger(w http.ResponseWriter, req *http.Request, trigger adminTriggerRequest) {
	route, ok := rt.adminCronPlanRoute(trigger.Expr)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	scheduledAt, err := trigger.scheduledTime()
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	rt.executeAdminTriggerCron(w, req, route, scheduledAt)
}

func (rt httpRuntime) serveAdminStreamTrigger(w http.ResponseWriter, req *http.Request, trigger adminTriggerRequest) {
	route, ok := rt.adminStreamPlanRoute(trigger.URI)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	body, err := trigger.bodyString()
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	rt.executeAdminTriggerStream(w, req, route, streamMessage{
		ID:       strings.TrimSpace(trigger.ID),
		Body:     body,
		Metadata: cleanAdminTriggerMetadata(trigger.Metadata),
	})
}

func (rt httpRuntime) adminTriggerRoute(method, path string) (httpRoute, map[string]string, bool) {
	for _, route := range rt.adminRoutes {
		if route.method == method && route.path == path {
			return route, nil, true
		}
	}
	for _, route := range rt.adminRoutes {
		if route.method != method {
			continue
		}
		pathParams, ok := matchHTTPRoutePath(route.path, path)
		if ok {
			return route, pathParams, true
		}
	}
	return httpRoute{}, nil, false
}

func (rt httpRuntime) adminCronPlanRoute(expr string) (adminPlanRoute, bool) {
	expr = strings.TrimSpace(expr)
	var match adminPlanRoute
	matches := 0
	for _, route := range rt.adminPlans {
		if route.plan.Trigger.Kind != runtimeplan.TriggerCron {
			continue
		}
		if expr != "" && route.plan.Trigger.Expr != expr {
			continue
		}
		match = route
		matches++
	}
	return match, matches == 1
}

func (rt httpRuntime) adminStreamPlanRoute(uri string) (adminPlanRoute, bool) {
	uri = strings.TrimSpace(uri)
	var match adminPlanRoute
	matches := 0
	for _, route := range rt.adminPlans {
		if route.plan.Trigger.Kind != runtimeplan.TriggerStream {
			continue
		}
		if uri != "" && route.plan.Trigger.URI != uri {
			continue
		}
		match = route
		matches++
	}
	return match, matches == 1
}

func matchHTTPRoutePath(routePath, actualPath string) (map[string]string, bool) {
	if routePath == actualPath {
		return nil, true
	}
	if !strings.HasPrefix(routePath, "/") || !strings.HasPrefix(actualPath, "/") {
		return nil, false
	}
	routeSegments := splitHTTPPath(routePath)
	actualSegments := splitHTTPPath(actualPath)
	if len(routeSegments) != len(actualSegments) {
		return nil, false
	}

	params := make(map[string]string)
	for i, routeSegment := range routeSegments {
		if name, ok := singleHTTPPathParamName(routeSegment); ok {
			params[name] = actualSegments[i]
			continue
		}
		if routeSegment != actualSegments[i] {
			return nil, false
		}
	}
	if len(params) == 0 {
		return nil, false
	}
	return params, true
}

func splitHTTPPath(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func singleHTTPPathParamName(segment string) (string, bool) {
	if len(segment) < 3 || !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
	if name == "" || strings.Contains(name, "...") {
		return "", false
	}
	return name, true
}

func (rt httpRuntime) executeAdminTriggerRoute(w http.ResponseWriter, req *http.Request, route httpRoute, input string) {
	if len(route.plan.Steps) == 0 {
		switch route.plan.Terminal.Kind {
		case runtimeplan.TerminalReply:
			if route.plan.Terminal.Async {
				writeJSONStatus(w, http.StatusAccepted, "accepted")
				return
			}
			if err := rt.validateDirectTerminalReplyOutput(req.Context(), route.plan); err != nil {
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
			writeJSONStatus(w, http.StatusOK, "ok")
		case runtimeplan.TerminalPush:
			if err := rt.applyPushTerminal(req.Context(), route.plan.Terminal, planRunResult{Output: input}, input); err != nil {
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
			writeJSONStatus(w, http.StatusAccepted, "accepted")
		case runtimeplan.TerminalSink:
			if err := rt.applySinkTerminal(req.Context(), route.plan.Terminal, planRunResult{Output: input}, "input"); err != nil {
				writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
				return
			}
			writeJSONStatus(w, http.StatusAccepted, "accepted")
		default:
			writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
		}
		return
	}

	if !route.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	if route.plan.Terminal.Async {
		if !rt.startAsync(func(ctx context.Context) {
			defer route.releaseWorker()
			_, _ = rt.runPlan(ctx, route.plan, input)
		}) {
			route.releaseWorker()
			writeJSONStatus(w, http.StatusServiceUnavailable, "shutting_down")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
		return
	}
	defer route.releaseWorker()

	result, err := rt.runPlanResult(req.Context(), route.plan, input)
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
	if err := rt.validateObservedTerminalReplyOutput(req.Context(), route.plan, result); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}
	output := result.Output

	switch route.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeJSONOutput(w, http.StatusOK, "ok", events.RedactJSONText(output))
	case runtimeplan.TerminalPush:
		if err := rt.applyPushTerminal(req.Context(), route.plan.Terminal, result, output); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONOutput(w, http.StatusAccepted, "accepted", events.RedactJSONText(output))
	case runtimeplan.TerminalSink:
		if err := rt.applySinkTerminal(req.Context(), route.plan.Terminal, result, "output"); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
			return
		}
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	default:
		writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
	}
}

func (rt httpRuntime) executeAdminTriggerCron(w http.ResponseWriter, req *http.Request, route adminPlanRoute, scheduledAt time.Time) {
	if !route.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	defer route.releaseWorker()

	result, err := runCronPlanOnce(req.Context(), rt, route.plan, scheduledAt)
	rt.writeAdminTriggerPlanResult(w, req, route.plan, result, err)
}

func (rt httpRuntime) executeAdminTriggerStream(w http.ResponseWriter, req *http.Request, route adminPlanRoute, message streamMessage) {
	if !route.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	defer route.releaseWorker()

	result, err := runStreamPlanOnce(req.Context(), rt, route.plan, message)
	rt.writeAdminTriggerPlanResult(w, req, route.plan, result, err)
}

func (rt httpRuntime) writeAdminTriggerPlanResult(w http.ResponseWriter, req *http.Request, plan runtimeplan.Plan, result planRunResult, err error) {
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
	if err := rt.validateObservedTerminalReplyOutput(req.Context(), plan, result); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}
	switch plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeJSONOutput(w, http.StatusOK, "ok", events.RedactJSONText(result.Output))
	case runtimeplan.TerminalPush:
		writeJSONOutput(w, http.StatusAccepted, "accepted", events.RedactJSONText(result.Output))
	case runtimeplan.TerminalSink:
		writeJSONStatus(w, http.StatusAccepted, "accepted")
	default:
		writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
	}
}

func (r adminPlanRoute) tryAcquireWorker() bool {
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

func (r adminPlanRoute) releaseWorker() {
	if r.workerPool == nil {
		return
	}
	<-r.workerPool
}

func (rt httpRuntime) authorizeAdmin(w http.ResponseWriter, req *http.Request) bool {
	token := strings.TrimSpace(rt.adminToken)
	if token == "" {
		if adminDevModeEnabled() {
			return true
		}
		writeJSONStatus(w, http.StatusUnauthorized, "admin_token_required")
		return false
	}
	auth := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		writeJSONStatus(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	provided := strings.TrimPrefix(auth, prefix)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
		writeJSONStatus(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

func adminDevModeEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("PIP_ENV")), "dev")
}

type adminHealthResponse struct {
	Status      string `json:"status"`
	StateStore  bool   `json:"state_store"`
	EventStream bool   `json:"event_stream"`
}

type adminStatusResponse struct {
	Status                 string         `json:"status"`
	Sessions               int            `json:"sessions"`
	Executions             int            `json:"executions"`
	ByStatus               map[string]int `json:"by_status"`
	Events                 int            `json:"events"`
	LLMCalls               int            `json:"llm_calls"`
	InputTokens            int            `json:"input_tokens"`
	OutputTokens           int            `json:"output_tokens"`
	CostUSD                float64        `json:"cost_usd"`
	AverageLatencyMS       float64        `json:"average_latency_ms"`
	SchemaViolations       int            `json:"schema_violations"`
	SchemaValidationPassed int            `json:"schema_validation_passed"`
	SchemaValidationFailed int            `json:"schema_validation_failed"`
	SchemaRepairsStarted   int            `json:"schema_repairs_started"`
	SchemaRepairsCompleted int            `json:"schema_repairs_completed"`
}

type adminLLMUsageSummary struct {
	Calls          int
	InputTokens    int
	OutputTokens   int
	CostUSD        float64
	latencySamples int
	latencyTotalMS float64
}

func (s *adminLLMUsageSummary) AddEvent(event events.Event) {
	if s == nil || events.CanonicalKind(event.Kind) != events.EventLLMCallCompleted {
		return
	}
	s.Calls++
	s.InputTokens += int(adminNumericPayload(event.Payload, "input_tokens"))
	s.OutputTokens += int(adminNumericPayload(event.Payload, "output_tokens"))
	s.CostUSD += adminNumericPayload(event.Payload, "cost_usd")
	latency := adminNumericPayload(event.Payload, "latency_ms")
	if latency > 0 {
		s.latencySamples++
		s.latencyTotalMS += latency
	}
}

func (s adminLLMUsageSummary) AverageLatencyMS() float64 {
	if s.latencySamples == 0 {
		return 0
	}
	return s.latencyTotalMS / float64(s.latencySamples)
}

type adminTracesResponse struct {
	Status string              `json:"status"`
	Traces []adminTraceSummary `json:"traces"`
}

type adminTraceSummary struct {
	ExecID       string    `json:"exec_id,omitempty"`
	TraceID      string    `json:"trace_id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	Events       int       `json:"events"`
	LLMCalls     int       `json:"llm_calls"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	CostUSD      float64   `json:"cost_usd"`
	LatencyMS    float64   `json:"average_latency_ms"`
	FirstEventID uint64    `json:"first_event_id"`
	LastEventID  uint64    `json:"last_event_id"`
	LastKind     string    `json:"last_kind,omitempty"`
	LastAt       time.Time `json:"last_at,omitempty"`
	llmUsage     adminLLMUsageSummary
}

type adminTraceResponse struct {
	Status           string                  `json:"status"`
	Execution        *adminExecutionResponse `json:"execution,omitempty"`
	Events           []adminEventResponse    `json:"events"`
	Sessions         int                     `json:"sessions"`
	SchemaViolations int                     `json:"schema_violations"`
}

type adminTriggerRequest struct {
	Trigger     string            `json:"trigger"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Expr        string            `json:"expr"`
	URI         string            `json:"uri"`
	ID          string            `json:"id"`
	ScheduledAt string            `json:"scheduled_at"`
	Metadata    map[string]string `json:"metadata"`
	Body        json.RawMessage   `json:"body"`
}

func (r adminTriggerRequest) kind() runtimeplan.TriggerKind {
	trigger := strings.ToLower(strings.TrimSpace(r.Trigger))
	switch trigger {
	case "http":
		return runtimeplan.TriggerHTTP
	case "webhook":
		return runtimeplan.TriggerWebhook
	case "cron":
		return runtimeplan.TriggerCron
	case "stream":
		return runtimeplan.TriggerStream
	case "":
		switch {
		case strings.TrimSpace(r.Method) != "" || strings.TrimSpace(r.Path) != "":
			return runtimeplan.TriggerHTTP
		case strings.TrimSpace(r.Expr) != "":
			return runtimeplan.TriggerCron
		case strings.TrimSpace(r.URI) != "":
			return runtimeplan.TriggerStream
		default:
			return ""
		}
	default:
		return ""
	}
}

func (r adminTriggerRequest) bodyString() (string, error) {
	body := strings.TrimSpace(string(r.Body))
	if body == "" || body == "null" {
		return "", nil
	}
	if strings.HasPrefix(body, `"`) {
		var text string
		if err := json.Unmarshal(r.Body, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	if !json.Valid(r.Body) {
		return "", errors.New("invalid trigger body")
	}
	return body, nil
}

func (r adminTriggerRequest) scheduledTime() (time.Time, error) {
	raw := strings.TrimSpace(r.ScheduledAt)
	if raw == "" {
		return time.Now().UTC(), nil
	}
	scheduledAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, err
	}
	return scheduledAt.UTC(), nil
}

func cleanAdminTriggerMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	cleaned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		key = strings.TrimSpace(key)
		if key != "" {
			cleaned[key] = value
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

type adminExecutionResponse struct {
	ExecID      string    `json:"exec_id"`
	TraceID     string    `json:"trace_id,omitempty"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type adminEventResponse struct {
	ID        uint64         `json:"id"`
	At        time.Time      `json:"at"`
	Kind      string         `json:"kind"`
	ExecID    string         `json:"exec_id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

func parseAdminTraceLimit(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return defaultAdminTraceLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return defaultAdminTraceLimit
	}
	if limit > maxAdminTraceLimit {
		return maxAdminTraceLimit
	}
	return limit
}

func summarizeAdminTraces(recorded []events.Event, limit int) []adminTraceSummary {
	byKey := make(map[string]*adminTraceSummary)
	order := make([]string, 0)
	for _, event := range recorded {
		key := adminTraceKey(event)
		summary, ok := byKey[key]
		if !ok {
			summary = &adminTraceSummary{
				ExecID:       event.ExecID,
				TraceID:      event.TraceID,
				SessionID:    event.SessionID,
				FirstEventID: event.ID,
			}
			byKey[key] = summary
			order = append(order, key)
		}
		summary.Events++
		summary.LastEventID = event.ID
		summary.LastKind = string(event.Kind)
		summary.LastAt = event.At
		summary.llmUsage.AddEvent(event)
		summary.LLMCalls = summary.llmUsage.Calls
		summary.InputTokens = summary.llmUsage.InputTokens
		summary.OutputTokens = summary.llmUsage.OutputTokens
		summary.CostUSD = summary.llmUsage.CostUSD
		summary.LatencyMS = summary.llmUsage.AverageLatencyMS()
		if summary.ExecID == "" {
			summary.ExecID = event.ExecID
		}
		if summary.TraceID == "" {
			summary.TraceID = event.TraceID
		}
		if summary.SessionID == "" {
			summary.SessionID = event.SessionID
		}
	}

	traces := make([]adminTraceSummary, 0, len(byKey))
	for _, key := range order {
		traces = append(traces, *byKey[key])
	}
	sort.SliceStable(traces, func(i, j int) bool {
		return traces[i].LastEventID < traces[j].LastEventID
	})
	if limit > 0 && len(traces) > limit {
		traces = traces[len(traces)-limit:]
	}
	return traces
}

func adminTraceKey(event events.Event) string {
	if event.ExecID != "" {
		return "exec:" + event.ExecID
	}
	if event.TraceID != "" {
		return "trace:" + event.TraceID
	}
	if event.SessionID != "" {
		return "session:" + event.SessionID
	}
	return fmt.Sprintf("event:%d", event.ID)
}

func adminExecutionResponseFromState(execution state.Execution) *adminExecutionResponse {
	return &adminExecutionResponse{
		ExecID:      execution.ExecID,
		TraceID:     execution.TraceID,
		Status:      string(execution.Status),
		StartedAt:   execution.StartedAt,
		CompletedAt: execution.CompletedAt,
	}
}

func adminEventResponseFromEvent(event events.Event) adminEventResponse {
	event = events.SanitizeEvent(event)
	return adminEventResponse{
		ID:        event.ID,
		At:        event.At,
		Kind:      string(event.Kind),
		ExecID:    event.ExecID,
		SessionID: event.SessionID,
		TraceID:   event.TraceID,
		Payload:   event.Payload,
	}
}
