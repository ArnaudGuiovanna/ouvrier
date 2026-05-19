package ovr

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

type adminExecutionLister interface {
	Executions(context.Context) ([]state.Execution, error)
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
		routes = append(routes, httpRoute{
			method:     plan.Trigger.Method,
			path:       plan.Trigger.Path,
			plan:       plan,
			workerPool: newWorkerPool(plan.Trigger.WorkerPool),
		})
	}
	return routes, nil
}

func newWorkerPool(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
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
	if rt.eventStream != nil {
		response.Events = len(rt.eventStream.List())
	}
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

	if lister, ok := rt.stateStore.(adminExecutionLister); ok {
		executions, err := lister.Executions(req.Context())
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		response.Executions = len(executions)
		for _, execution := range executions {
			response.ByStatus[string(execution.Status)]++
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	seenExecs := make(map[string]struct{})
	for _, session := range sessions {
		if session.ExecID == "" {
			continue
		}
		if _, seen := seenExecs[session.ExecID]; seen {
			continue
		}
		seenExecs[session.ExecID] = struct{}{}

		execution, ok, err := rt.stateStore.Execution(req.Context(), session.ExecID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		if !ok {
			continue
		}
		response.Executions++
		response.ByStatus[string(execution.Status)]++
	}
	writeJSON(w, http.StatusOK, response)
}

func (rt httpRuntime) serveAdminTraces(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	events := []events.Event{}
	if rt.eventStream != nil {
		events = rt.eventStream.List()
	}
	traces := summarizeAdminTraces(events, parseAdminTraceLimit(req.URL.Query().Get("last")))
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

	if rt.eventStream != nil {
		for _, event := range rt.eventStream.List() {
			if event.ExecID == execID {
				response.Events = append(response.Events, adminEventResponseFromEvent(event))
			}
		}
	}
	if response.Execution == nil && len(response.Events) == 0 {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}

	writeJSON(w, http.StatusOK, response)
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
	input, err := buildHTTPPipelineInput(body, pathParams)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trigger")
		return
	}
	rt.executeAdminTriggerRoute(w, req, route, input)
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
			writeJSONStatus(w, http.StatusOK, "ok")
		case runtimeplan.TerminalPush, runtimeplan.TerminalSink:
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
		ctx := context.WithoutCancel(req.Context())
		go func() {
			defer route.releaseWorker()
			_, _ = rt.runPlan(ctx, route.plan, input)
		}()
		writeJSONStatus(w, http.StatusAccepted, "accepted")
		return
	}
	defer route.releaseWorker()

	output, err := rt.runPlan(req.Context(), route.plan, input)
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
	if err := validateTerminalReplyOutput(route.plan, output); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, "pipeline_execution_failed")
		return
	}

	switch route.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeJSONOutput(w, http.StatusOK, "ok", output)
	case runtimeplan.TerminalPush, runtimeplan.TerminalSink:
		writeJSONOutput(w, http.StatusAccepted, "accepted", output)
	default:
		writeJSONStatus(w, http.StatusInternalServerError, "terminal_missing")
	}
}

func (rt httpRuntime) authorizeAdmin(w http.ResponseWriter, req *http.Request) bool {
	token := strings.TrimSpace(rt.adminToken)
	if token == "" {
		return true
	}
	auth := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		writeJSONStatus(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	provided := strings.TrimPrefix(auth, prefix)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
		writeJSONStatus(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

type adminHealthResponse struct {
	Status      string `json:"status"`
	StateStore  bool   `json:"state_store"`
	EventStream bool   `json:"event_stream"`
}

type adminStatusResponse struct {
	Status     string         `json:"status"`
	Sessions   int            `json:"sessions"`
	Executions int            `json:"executions"`
	ByStatus   map[string]int `json:"by_status"`
	Events     int            `json:"events"`
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
	FirstEventID uint64    `json:"first_event_id"`
	LastEventID  uint64    `json:"last_event_id"`
	LastKind     string    `json:"last_kind,omitempty"`
	LastAt       time.Time `json:"last_at,omitempty"`
}

type adminTraceResponse struct {
	Status           string                  `json:"status"`
	Execution        *adminExecutionResponse `json:"execution,omitempty"`
	Events           []adminEventResponse    `json:"events"`
	Sessions         int                     `json:"sessions"`
	SchemaViolations int                     `json:"schema_violations"`
}

type adminTriggerRequest struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
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
