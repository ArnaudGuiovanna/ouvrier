package ovr

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
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
	mux.HandleFunc("GET /admin/plans", rt.serveAdminPlans)
	mux.HandleFunc("GET /admin/capabilities", rt.serveAdminCapabilities)
	mux.HandleFunc("GET /admin/events", rt.serveAdminEvents)
	mux.HandleFunc("GET /admin/traces", rt.serveAdminTraces)
	mux.HandleFunc("GET /admin/traces/{execID}", rt.serveAdminTrace)
	mux.HandleFunc("POST /admin/trigger", rt.serveAdminTrigger)
	mux.HandleFunc("GET /admin/approvals", rt.serveAdminApprovals)
	mux.HandleFunc("POST /admin/approvals/{id}", rt.serveAdminApprovalDecision)
	mux.HandleFunc("GET /admin/runs", rt.serveAdminRuns)
	mux.HandleFunc("POST /admin/runs/{execID}/recover", rt.serveAdminRunRecover)
	mux.HandleFunc("POST /admin/streams/replay", rt.serveAdminStreamReplay)
	mux.HandleFunc("GET /metrics", rt.serveMetrics)
	registerHTTPDevRoutes(mux, rt)
}

func (rt httpRuntime) serveAdminHealth(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	response := adminHealthResponse{
		Status:      "ok",
		StateStore:  rt.stateStore != nil,
		EventStream: rt.eventStream != nil,
	}
	if rt.stateStore != nil {
		executions, err := rt.stateStore.Executions(req.Context())
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		response.Executions = len(executions)
		response.RecentExecutions = recentAdminExecutions(executions, parseAdminTraceLimit(req.URL.Query().Get("last")))
	}
	if source, ok := rt.stateStore.(interface{ DBStats() sql.DBStats }); ok {
		stats := source.DBStats()
		response.StateDB = &adminStateDBResponse{
			MaxOpenConnections: stats.MaxOpenConnections,
			OpenConnections:    stats.OpenConnections,
			InUse:              stats.InUse,
			Idle:               stats.Idle,
			WaitCount:          stats.WaitCount,
			WaitDurationMS:     stats.WaitDuration.Milliseconds(),
		}
	}
	cronLeases, err := adminCronLeases(req.Context(), rt)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.CronLeases = cronLeases
	durableRuns, err := adminDurableRuns(req.Context(), rt)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.DurableRuns = durableRuns
	writeJSON(w, http.StatusOK, response)
}

// adminDurableRuns reports the durable-run journal posture when
// OUVRIER_DURABLE_RUNS is on, in the same additive spirit as the cron_leases
// section: retained journal count plus prune-failure observability.
func adminDurableRuns(ctx context.Context, rt httpRuntime) (*adminDurableRunsResponse, error) {
	if rt.durableRuns == nil || rt.stateStore == nil {
		return nil, nil
	}
	journals, err := rt.stateStore.RunJournals(ctx)
	if err != nil {
		return nil, err
	}
	failures, lastError, lastErrorAt := rt.durableRuns.health.snapshot()
	response := &adminDurableRunsResponse{
		Enabled:        true,
		Retention:      rt.durableRuns.retention.String(),
		Journals:       len(journals),
		PruneFailures:  failures,
		LastPruneError: lastError,
	}
	if !lastErrorAt.IsZero() {
		response.LastPruneErrorAt = lastErrorAt.UTC().Format(time.RFC3339Nano)
	}
	return response, nil
}

// adminCronLeases lists the cron leader-leases stored on lease-capable
// backends, in the same additive spirit as the state_db pool-stats section.
// is_self marks rows held under this replica's holder identity.
func adminCronLeases(ctx context.Context, rt httpRuntime) ([]adminCronLeaseResponse, error) {
	leaseStore, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		return nil, nil
	}
	leases, err := leaseStore.Leases(ctx)
	if err != nil {
		return nil, err
	}
	self := cronReplicaID()
	var rows []adminCronLeaseResponse
	for _, lease := range leases {
		if !strings.HasPrefix(lease.Name, "cron:") {
			continue
		}
		rows = append(rows, adminCronLeaseResponse{
			Name:      lease.Name,
			Holder:    lease.Holder,
			Fence:     lease.Fence,
			ExpiresAt: lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
			IsSelf:    lease.Holder == self,
		})
	}
	return rows, nil
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
	response.SchemaRepairsFailed = countAdminEventsByKind(recorded, events.EventSchemaRepairFailed)
	response.SchemaViolations = response.SchemaValidationFailed
	llm := summarizeAdminLLMUsage(recorded)
	response.LLMCalls = llm.Calls
	response.LLMFailures = llm.Failures
	response.LLMRetries = llm.Retries
	response.LLMFinalFailures = llm.FinalFailures
	response.InputTokens = llm.InputTokens
	response.OutputTokens = llm.OutputTokens
	response.CostUSD = llm.CostUSD
	response.AverageLatencyMS = llm.AverageLatencyMS()
	harnessMetrics := summarizeAdminHarnessMetrics(recorded)
	response.SessionsStarted = harnessMetrics.SessionsStarted
	response.SessionsCompleted = harnessMetrics.SessionsCompleted
	response.SessionsCancelled = harnessMetrics.SessionsCancelled
	response.ToolCalls = harnessMetrics.ToolCalls
	response.ToolCallsCompleted = harnessMetrics.ToolCallsCompleted
	response.ToolFailures = harnessMetrics.ToolFailures
	response.TasksStarted = harnessMetrics.TasksStarted
	response.TasksCompleted = harnessMetrics.TasksCompleted
	response.TasksFailed = harnessMetrics.TasksFailed
	response.PermissionAllowed = harnessMetrics.PermissionAllowed
	response.PermissionDenied = harnessMetrics.PermissionDenied
	response.BudgetExceeded = harnessMetrics.BudgetExceeded
	response.BudgetExceededTokens = harnessMetrics.BudgetExceededTokens
	response.BudgetExceededCostUSD = harnessMetrics.BudgetExceededCostUSD
	response.BudgetExceededWallClock = harnessMetrics.BudgetExceededWallClock
	response.BudgetExceededIterations = harnessMetrics.BudgetExceededIterations
	response.HookFailures = harnessMetrics.HookFailures
	decisionMetrics := summarizeAdminDecisionMetrics(recorded)
	response.SignatureDecisions = decisionMetrics.SignatureDecisions
	response.SignatureValid = decisionMetrics.SignatureValid
	response.SignatureInvalid = decisionMetrics.SignatureInvalid
	response.SignatureMissing = decisionMetrics.SignatureMissing
	response.SignatureSecretMissing = decisionMetrics.SignatureSecretMissing
	response.IdempotencyDecisions = decisionMetrics.IdempotencyDecisions
	response.IdempotencyReserved = decisionMetrics.IdempotencyReserved
	response.IdempotencyDuplicate = decisionMetrics.IdempotencyDuplicate
	response.IdempotencyRetry = decisionMetrics.IdempotencyRetry
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
	response.RecentSchemaViolations = recentAdminSchemaViolations(violations, parseAdminTraceLimit(req.URL.Query().Get("last")))
	cronLeases, err := adminCronLeases(req.Context(), rt)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	response.CronLeases = cronLeases
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
	selector, err := parseAdminTraceSelector(execID)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trace")
		return
	}
	afterID, err := parseAdminTraceAfterID(req.URL.Query().Get("after_id"))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_trace_cursor")
		return
	}

	response := adminTraceResponse{Status: "ok"}
	knownTrace := false
	if rt.stateStore != nil && selector.kind == adminTraceSelectorExec {
		execution, ok, err := rt.stateStore.Execution(req.Context(), selector.value)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		if ok {
			response.Execution = adminExecutionResponseFromState(execution)
			knownTrace = true
		}

		sessions, err := rt.stateStore.Sessions(req.Context())
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		traceSessions := make([]runtimeplan.Session, 0)
		for _, session := range sessions {
			if session.ExecID == selector.value {
				traceSessions = append(traceSessions, session)
			}
		}
		sort.Slice(traceSessions, func(i, j int) bool {
			if !traceSessions[i].StartedAt.Equal(traceSessions[j].StartedAt) {
				return traceSessions[i].StartedAt.Before(traceSessions[j].StartedAt)
			}
			return traceSessions[i].SessionID < traceSessions[j].SessionID
		})
		response.Sessions = len(traceSessions)
		response.SessionDetails = make([]adminSessionResponse, 0, len(traceSessions))
		for _, session := range traceSessions {
			response.SessionDetails = append(response.SessionDetails, adminSessionResponseFromRuntime(session))
		}
		if response.Sessions > 0 {
			knownTrace = true
		}

		violations, err := rt.stateStore.SchemaViolations(req.Context(), selector.value)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		response.SchemaViolations = len(violations)
		response.SchemaViolationDetails = make([]adminSchemaViolationResponse, 0, len(violations))
		for _, violation := range violations {
			response.SchemaViolationDetails = append(response.SchemaViolationDetails, adminSchemaViolationResponseFromState(violation))
		}
		if response.SchemaViolations > 0 {
			knownTrace = true
		}
	}

	recorded, err := rt.adminEventsForSelectorSince(req.Context(), selector, afterID)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	for _, event := range recorded {
		response.Events = append(response.Events, adminEventResponseFromEvent(event))
		knownTrace = true
		if event.ID > response.LastEventID {
			response.LastEventID = event.ID
		}
	}
	if afterID > 0 && len(recorded) == 0 && !knownTrace {
		var lastExistingID uint64
		knownTrace, lastExistingID, err = rt.adminTraceKnown(req.Context(), selector)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
			return
		}
		if lastExistingID > response.LastEventID {
			response.LastEventID = lastExistingID
		}
	}
	if afterID > 0 && knownTrace && response.LastEventID < afterID {
		response.LastEventID = afterID
	}
	if !knownTrace {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}

	writeJSON(w, http.StatusOK, response)
}

func (rt httpRuntime) adminEvents(ctx context.Context, execID string) ([]events.Event, error) {
	return rt.adminEventsSince(ctx, execID, 0)
}

func (rt httpRuntime) adminEventsSince(ctx context.Context, execID string, afterID uint64) ([]events.Event, error) {
	if rt.stateStore != nil {
		recorded, err := rt.stateStore.EventsSince(ctx, execID, afterID)
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
	recorded := rt.eventStream.Since(afterID)
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

func (rt httpRuntime) adminEventsForSelectorSince(ctx context.Context, selector adminTraceSelector, afterID uint64) ([]events.Event, error) {
	if selector.kind == adminTraceSelectorExec {
		return rt.adminEventsSince(ctx, selector.value, afterID)
	}
	recorded, err := rt.adminEventsSince(ctx, "", afterID)
	if err != nil {
		return nil, err
	}
	filtered := make([]events.Event, 0, len(recorded))
	for _, event := range recorded {
		if selector.matches(event) {
			filtered = append(filtered, event)
		}
	}
	return filtered, nil
}

func (rt httpRuntime) adminTraceKnown(ctx context.Context, selector adminTraceSelector) (bool, uint64, error) {
	recorded, err := rt.adminEventsForSelectorSince(ctx, selector, 0)
	if err != nil {
		return false, 0, err
	}
	var lastID uint64
	for _, event := range recorded {
		if event.ID > lastID {
			lastID = event.ID
		}
	}
	return len(recorded) > 0, lastID, nil
}

type adminTraceSelectorKind string

const (
	adminTraceSelectorExec    adminTraceSelectorKind = "exec"
	adminTraceSelectorTrace   adminTraceSelectorKind = "trace"
	adminTraceSelectorSession adminTraceSelectorKind = "session"
	adminTraceSelectorEvent   adminTraceSelectorKind = "event"
)

type adminTraceSelector struct {
	kind    adminTraceSelectorKind
	value   string
	eventID uint64
}

func parseAdminTraceSelector(raw string) (adminTraceSelector, error) {
	raw = strings.TrimSpace(raw)
	prefix, value, hasPrefix := strings.Cut(raw, ":")
	if !hasPrefix {
		return adminTraceSelector{kind: adminTraceSelectorExec, value: raw}, nil
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return adminTraceSelector{}, errors.New("trace selector value is required")
	}
	switch prefix {
	case string(adminTraceSelectorExec):
		return adminTraceSelector{kind: adminTraceSelectorExec, value: value}, nil
	case string(adminTraceSelectorTrace):
		return adminTraceSelector{kind: adminTraceSelectorTrace, value: value}, nil
	case string(adminTraceSelectorSession):
		return adminTraceSelector{kind: adminTraceSelectorSession, value: value}, nil
	case string(adminTraceSelectorEvent):
		eventID, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return adminTraceSelector{}, err
		}
		return adminTraceSelector{kind: adminTraceSelectorEvent, value: value, eventID: eventID}, nil
	default:
		return adminTraceSelector{kind: adminTraceSelectorExec, value: raw}, nil
	}
}

func (s adminTraceSelector) matches(event events.Event) bool {
	switch s.kind {
	case adminTraceSelectorExec:
		return event.ExecID == s.value
	case adminTraceSelectorTrace:
		return event.TraceID == s.value
	case adminTraceSelectorSession:
		return event.SessionID == s.value
	case adminTraceSelectorEvent:
		return event.ID == s.eventID
	default:
		return false
	}
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

type adminHarnessMetricsSummary struct {
	SessionsStarted          int
	SessionsCompleted        int
	SessionsCancelled        int
	ToolCalls                int
	ToolCallsCompleted       int
	ToolFailures             int
	TasksStarted             int
	TasksCompleted           int
	TasksFailed              int
	PermissionAllowed        int
	PermissionDenied         int
	BudgetExceeded           int
	BudgetExceededTokens     int
	BudgetExceededCostUSD    int
	BudgetExceededWallClock  int
	BudgetExceededIterations int
	HookFailures             int
}

type adminDecisionMetricsSummary struct {
	SignatureDecisions     int
	SignatureValid         int
	SignatureInvalid       int
	SignatureMissing       int
	SignatureSecretMissing int
	IdempotencyDecisions   int
	IdempotencyReserved    int
	IdempotencyDuplicate   int
	IdempotencyRetry       int
}

func summarizeAdminHarnessMetrics(recorded []events.Event) adminHarnessMetricsSummary {
	var summary adminHarnessMetricsSummary
	for _, event := range recorded {
		summary.AddEvent(event)
	}
	return summary
}

func summarizeAdminDecisionMetrics(recorded []events.Event) adminDecisionMetricsSummary {
	var summary adminDecisionMetricsSummary
	for _, event := range recorded {
		summary.AddEvent(event)
	}
	return summary
}

func (s *adminDecisionMetricsSummary) AddEvent(event events.Event) {
	if s == nil {
		return
	}
	decision := adminStringPayload(event.Payload, "decision")
	switch events.CanonicalKind(event.Kind) {
	case events.EventSignatureDecision:
		s.SignatureDecisions++
		switch decision {
		case "valid":
			s.SignatureValid++
		case "invalid":
			s.SignatureInvalid++
		case "missing":
			s.SignatureMissing++
		case "secret_missing":
			s.SignatureSecretMissing++
		}
	case events.EventIdempotencyDecision:
		s.IdempotencyDecisions++
		switch decision {
		case "reserved":
			s.IdempotencyReserved++
		case "duplicate":
			s.IdempotencyDuplicate++
		case "retry":
			s.IdempotencyRetry++
		}
	}
}

func (s *adminHarnessMetricsSummary) AddEvent(event events.Event) {
	if s == nil {
		return
	}
	switch events.CanonicalKind(event.Kind) {
	case events.EventSessionStarted:
		s.SessionsStarted++
	case events.EventSessionSaved:
		if adminStringPayload(event.Payload, "status") == "completed" {
			s.SessionsCompleted++
		}
	case events.EventSessionCancelled:
		s.SessionsCancelled++
	case events.EventToolCallStarted:
		s.ToolCalls++
	case events.EventToolCallCompleted:
		s.ToolCallsCompleted++
	case events.EventToolCallFailed:
		s.ToolFailures++
	case events.EventTaskStarted:
		s.TasksStarted++
	case events.EventTaskCompleted:
		s.TasksCompleted++
	case events.EventTaskFailed:
		s.TasksFailed++
	case events.EventPermissionDecision:
		if adminBoolPayload(event.Payload, "allowed") {
			s.PermissionAllowed++
		} else {
			s.PermissionDenied++
		}
	case events.EventBudgetExceeded:
		s.BudgetExceeded++
		switch adminStringPayload(event.Payload, "budget") {
		case "tokens":
			s.BudgetExceededTokens++
		case "cost_usd":
			s.BudgetExceededCostUSD++
		case "wallclock":
			s.BudgetExceededWallClock++
		case "iterations":
			s.BudgetExceededIterations++
		}
	case events.EventHookFailed:
		s.HookFailures++
	}
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

func adminBoolPayload(payload map[string]any, key string) bool {
	if payload == nil {
		return false
	}
	value, ok := payload[key].(bool)
	return ok && value
}

func adminStringPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key].(string)
	if !ok {
		return ""
	}
	return value
}

func (rt httpRuntime) serveAdminTrigger(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	limitAdminBody(w, req)
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
		if !route.tryAcquireWorker() {
			writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
			return
		}
		defer route.releaseWorker()

		result, err := rt.startDirectPlanExecution(req.Context(), route.plan, input, nil)
		if err != nil {
			writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
			return
		}
		switch route.plan.Terminal.Kind {
		case runtimeplan.TerminalReply:
			if route.plan.Terminal.Async {
				if err := rt.finishDirectPlanExecution(req.Context(), route.plan, result, nil); err != nil {
					writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
					return
				}
				writeAdminTriggerStatus(w, http.StatusAccepted, "accepted", result)
				return
			}
			result.Output = directReplyOKOutput
			if err := rt.validateObservedTerminalReplyOutput(req.Context(), route.plan, result); err != nil {
				_ = rt.finishDirectPlanExecution(req.Context(), route.plan, result, err)
				writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
				return
			}
			if err := rt.finishDirectPlanExecution(req.Context(), route.plan, result, nil); err != nil {
				writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
				return
			}
			writeAdminTriggerStatus(w, http.StatusOK, "ok", result)
		case runtimeplan.TerminalPush:
			if err := rt.applyPushTerminal(req.Context(), route.plan.Terminal, result, input); err != nil {
				_ = rt.finishDirectPlanExecution(req.Context(), route.plan, result, err)
				writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
				return
			}
			if err := rt.finishDirectPlanExecution(req.Context(), route.plan, result, nil); err != nil {
				writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
				return
			}
			writeAdminTriggerStatus(w, http.StatusAccepted, "accepted", result)
		case runtimeplan.TerminalSink:
			if err := rt.applySinkTerminal(req.Context(), route.plan.Terminal, result, "input"); err != nil {
				_ = rt.finishDirectPlanExecution(req.Context(), route.plan, result, err)
				writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
				return
			}
			if err := rt.finishDirectPlanExecution(req.Context(), route.plan, result, nil); err != nil {
				writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
				return
			}
			writeAdminTriggerStatus(w, http.StatusAccepted, "accepted", result)
		default:
			_ = rt.finishDirectPlanExecution(req.Context(), route.plan, result, errors.New("terminal missing"))
			writeAdminTriggerStatus(w, http.StatusInternalServerError, "terminal_missing", result)
		}
		return
	}

	if !route.tryAcquireWorker() {
		writeJSONStatus(w, http.StatusTooManyRequests, "worker_pool_full")
		return
	}
	if route.plan.Terminal.Async {
		session, err := pipelineSessionForPlan(route.plan, nil)
		result := planRunResult{Session: session, HasSession: err == nil}
		if err != nil {
			route.releaseWorker()
			writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
			return
		}
		if !rt.startAsync(func(ctx context.Context) {
			defer route.releaseWorker()
			_, _ = rt.runPlanWithSession(ctx, route.plan, input, &session)
		}) {
			route.releaseWorker()
			writeAdminTriggerStatus(w, http.StatusServiceUnavailable, "shutting_down", result)
			return
		}
		writeAdminTriggerStatus(w, http.StatusAccepted, "accepted", result)
		return
	}
	defer route.releaseWorker()

	result, err := rt.runPlanResult(req.Context(), route.plan, input)
	if err != nil {
		if suspended, ok := suspendedExecutionError(err); ok {
			writeSuspendedResponse(w, suspended)
			return
		}
		writeAdminTriggerPlanError(w, result, err)
		return
	}
	if err := rt.validateObservedTerminalReplyOutput(req.Context(), route.plan, result); err != nil {
		writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
		return
	}
	output := result.Output

	switch route.plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeAdminTriggerOutput(w, http.StatusOK, "ok", result, events.RedactJSONText(output))
	case runtimeplan.TerminalPush:
		if err := rt.applyPushTerminal(req.Context(), route.plan.Terminal, result, output); err != nil {
			writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
			return
		}
		writeAdminTriggerOutput(w, http.StatusAccepted, "accepted", result, events.RedactJSONText(output))
	case runtimeplan.TerminalSink:
		if err := rt.applySinkTerminal(req.Context(), route.plan.Terminal, result, "output"); err != nil {
			writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
			return
		}
		writeAdminTriggerStatus(w, http.StatusAccepted, "accepted", result)
	default:
		writeAdminTriggerStatus(w, http.StatusInternalServerError, "terminal_missing", result)
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
		writeAdminTriggerPlanError(w, result, err)
		return
	}
	if err := rt.validateObservedTerminalReplyOutput(req.Context(), plan, result); err != nil {
		writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
		return
	}
	switch plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		writeAdminTriggerOutput(w, http.StatusOK, "ok", result, events.RedactJSONText(result.Output))
	case runtimeplan.TerminalPush:
		writeAdminTriggerOutput(w, http.StatusAccepted, "accepted", result, events.RedactJSONText(result.Output))
	case runtimeplan.TerminalSink:
		writeAdminTriggerStatus(w, http.StatusAccepted, "accepted", result)
	default:
		writeAdminTriggerStatus(w, http.StatusInternalServerError, "terminal_missing", result)
	}
}

func writeAdminTriggerPlanError(w http.ResponseWriter, result planRunResult, err error) {
	switch {
	case errors.Is(err, errHTTPProviderNotConfigured):
		writeAdminTriggerStatus(w, http.StatusServiceUnavailable, "provider_not_configured", result)
	case errors.Is(err, errHTTPPipelineIncomplete):
		writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_incomplete", result)
	default:
		writeAdminTriggerStatus(w, http.StatusBadGateway, "pipeline_execution_failed", result)
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
	return strings.EqualFold(strings.TrimSpace(os.Getenv(envnames.Env)), "dev")
}

const adminInsecureEnv = "OUVRIER_ADMIN_INSECURE"

// envOptIn reports whether the named environment variable carries an explicit
// truthy opt-in value.
func envOptIn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// adminInsecureOptIn reports whether the operator explicitly accepted serving
// unauthenticated admin endpoints on a network-reachable address.
func adminInsecureOptIn() bool {
	return envOptIn(adminInsecureEnv)
}

// adminAddrFromEnv returns the OUVRIER_ADMIN_ADDR bind address for the
// dedicated admin listener, or "" when the admin surface stays on the public
// port (the v0.2 shared-port layout).
func adminAddrFromEnv() string {
	return strings.TrimSpace(os.Getenv(envnames.AdminAddr))
}

// metricsPublicOptIn reports whether OUVRIER_METRICS_PUBLIC keeps /metrics
// registered on the public port even when OUVRIER_ADMIN_ADDR moves the admin
// surface to its own listener, for Prometheus scrapers that cannot traverse
// SSH to the loopback admin port. It parses the same truthy values as
// OUVRIER_ADMIN_INSECURE and has no effect while OUVRIER_ADMIN_ADDR is unset.
func metricsPublicOptIn() bool {
	return envOptIn(envnames.MetricsPublic)
}

// loopbackBindAddr reports whether addr binds only the loopback interface. An
// empty host (e.g. ":8080") means every interface and is therefore not
// loopback-only.
func loopbackBindAddr(addr string) bool {
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

// checkAdminExposure guards the production server entrypoint against
// unauthenticated admin endpoints reachable from the network.
//
// When an admin token is set, bearer auth is enforced and any bind is fine.
// When no token is set and dev mode is off, every /admin/* route answers 401,
// so the surface is locked. The dangerous combination is dev mode + no token,
// which disables admin auth entirely: that is permitted only on a loopback bind
// or with an explicit OUVRIER_ADMIN_INSECURE opt-in; otherwise the server
// refuses to start rather than silently exposing /admin/trigger and approval
// decisions to the network.
func checkAdminExposure(addr, adminToken string) error {
	if strings.TrimSpace(adminToken) != "" {
		return nil
	}
	if !adminDevModeEnabled() {
		return nil
	}
	if loopbackBindAddr(addr) || adminInsecureOptIn() {
		return nil
	}
	return fmt.Errorf("refusing to start: admin endpoints are unauthenticated (%s=dev, no %s) and %q is reachable from the network; set %s for production, bind to localhost for local dev, or set %s=1 to override", envnames.Env, envnames.AdminToken, addr, envnames.AdminToken, adminInsecureEnv)
}

// checkAdminAddrExposure guards the dedicated admin listener requested via
// OUVRIER_ADMIN_ADDR. The split listener exists so the admin surface is never
// reachable from the network (operators tunnel to it over SSH), so a
// non-loopback admin bind defeats its purpose and is refused regardless of
// token or dev mode — unless the operator explicitly opts in with the same
// OUVRIER_ADMIN_INSECURE override that checkAdminExposure honors for the
// shared-port layout.
func checkAdminAddrExposure(adminAddr string) error {
	if loopbackBindAddr(adminAddr) || adminInsecureOptIn() {
		return nil
	}
	return fmt.Errorf("refusing to start: %s=%q binds the dedicated admin listener to a non-loopback address reachable from the network; bind it to 127.0.0.1, or set %s=1 to override", envnames.AdminAddr, adminAddr, adminInsecureEnv)
}

// adminAddrExposureWarning returns a non-empty warning when the dedicated
// admin listener was permitted on a non-loopback bind via the insecure
// opt-in, so the operator is told loudly at startup. It returns "" for a
// loopback-only bind.
func adminAddrExposureWarning(adminAddr string) string {
	if loopbackBindAddr(adminAddr) {
		return ""
	}
	return fmt.Sprintf("WARNING: dedicated admin listener %s=%q is reachable from the network (%s=1)", envnames.AdminAddr, adminAddr, adminInsecureEnv)
}

// adminExposureWarning returns a non-empty warning when admin auth is disabled
// on a permitted but still insecure configuration, so the operator is told
// loudly at startup. It returns "" when the configuration is safe.
func adminExposureWarning(addr, adminToken string) string {
	if strings.TrimSpace(adminToken) != "" || !adminDevModeEnabled() {
		return ""
	}
	return fmt.Sprintf("WARNING: admin endpoints are UNAUTHENTICATED (%s=dev, no %s) on %q", envnames.Env, envnames.AdminToken, addr)
}

type adminHealthResponse struct {
	Status           string                    `json:"status"`
	StateStore       bool                      `json:"state_store"`
	EventStream      bool                      `json:"event_stream"`
	Executions       int                       `json:"executions,omitempty"`
	RecentExecutions []adminExecutionResponse  `json:"recent_executions,omitempty"`
	StateDB          *adminStateDBResponse     `json:"state_db,omitempty"`
	CronLeases       []adminCronLeaseResponse  `json:"cron_leases,omitempty"`
	DurableRuns      *adminDurableRunsResponse `json:"durable_runs,omitempty"`
}

// adminDurableRunsResponse reports the durable-run journal posture when
// OUVRIER_DURABLE_RUNS is on: retained journal rows (failed/suspended/
// in-flight runs) and prune-failure observability.
type adminDurableRunsResponse struct {
	Enabled          bool   `json:"enabled"`
	Retention        string `json:"retention"`
	Journals         int    `json:"journals"`
	PruneFailures    uint64 `json:"prune_failures"`
	LastPruneError   string `json:"last_prune_error,omitempty"`
	LastPruneErrorAt string `json:"last_prune_error_at,omitempty"`
}

// adminCronLeaseResponse reports one cron leader-lease row for lease-capable
// state backends, expired rows included; consumers compare expires_at.
type adminCronLeaseResponse struct {
	Name      string `json:"name"`
	Holder    string `json:"holder"`
	Fence     uint64 `json:"fence"`
	ExpiresAt string `json:"expires_at"`
	IsSelf    bool   `json:"is_self"`
}

// adminStateDBResponse reports connection-pool statistics for state backends
// that expose them (currently only Postgres).
type adminStateDBResponse struct {
	MaxOpenConnections int   `json:"max_open_connections"`
	OpenConnections    int   `json:"open_connections"`
	InUse              int   `json:"in_use"`
	Idle               int   `json:"idle"`
	WaitCount          int64 `json:"wait_count"`
	WaitDurationMS     int64 `json:"wait_duration_ms"`
}

type adminStatusResponse struct {
	Status                   string                         `json:"status"`
	Sessions                 int                            `json:"sessions"`
	Executions               int                            `json:"executions"`
	ByStatus                 map[string]int                 `json:"by_status"`
	Events                   int                            `json:"events"`
	SessionsStarted          int                            `json:"sessions_started"`
	SessionsCompleted        int                            `json:"sessions_completed"`
	SessionsCancelled        int                            `json:"sessions_cancelled"`
	LLMCalls                 int                            `json:"llm_calls"`
	LLMFailures              int                            `json:"llm_failures"`
	LLMRetries               int                            `json:"llm_retries"`
	LLMFinalFailures         int                            `json:"llm_final_failures"`
	InputTokens              int                            `json:"input_tokens"`
	OutputTokens             int                            `json:"output_tokens"`
	CostUSD                  float64                        `json:"cost_usd"`
	AverageLatencyMS         float64                        `json:"average_latency_ms"`
	ToolCalls                int                            `json:"tool_calls"`
	ToolCallsCompleted       int                            `json:"tool_calls_completed"`
	ToolFailures             int                            `json:"tool_failures"`
	TasksStarted             int                            `json:"tasks_started"`
	TasksCompleted           int                            `json:"tasks_completed"`
	TasksFailed              int                            `json:"tasks_failed"`
	PermissionAllowed        int                            `json:"permission_allowed"`
	PermissionDenied         int                            `json:"permission_denied"`
	SchemaViolations         int                            `json:"schema_violations"`
	SchemaValidationPassed   int                            `json:"schema_validation_passed"`
	SchemaValidationFailed   int                            `json:"schema_validation_failed"`
	SchemaRepairsStarted     int                            `json:"schema_repairs_started"`
	SchemaRepairsCompleted   int                            `json:"schema_repairs_completed"`
	SchemaRepairsFailed      int                            `json:"schema_repairs_failed"`
	RecentSchemaViolations   []adminSchemaViolationResponse `json:"recent_schema_violations,omitempty"`
	BudgetExceeded           int                            `json:"budget_exceeded"`
	BudgetExceededTokens     int                            `json:"budget_exceeded_tokens"`
	BudgetExceededCostUSD    int                            `json:"budget_exceeded_cost_usd"`
	BudgetExceededWallClock  int                            `json:"budget_exceeded_wallclock"`
	BudgetExceededIterations int                            `json:"budget_exceeded_iterations"`
	HookFailures             int                            `json:"hook_failures"`
	SignatureDecisions       int                            `json:"signature_decisions"`
	SignatureValid           int                            `json:"signature_valid"`
	SignatureInvalid         int                            `json:"signature_invalid"`
	SignatureMissing         int                            `json:"signature_missing"`
	SignatureSecretMissing   int                            `json:"signature_secret_missing"`
	IdempotencyDecisions     int                            `json:"idempotency_decisions"`
	IdempotencyReserved      int                            `json:"idempotency_reserved"`
	IdempotencyDuplicate     int                            `json:"idempotency_duplicate"`
	IdempotencyRetry         int                            `json:"idempotency_retry"`
	CronLeases               []adminCronLeaseResponse       `json:"cron_leases,omitempty"`
}

type adminLLMUsageSummary struct {
	Calls          int
	Failures       int
	Retries        int
	FinalFailures  int
	InputTokens    int
	OutputTokens   int
	CostUSD        float64
	latencySamples int
	latencyTotalMS float64
}

func (s *adminLLMUsageSummary) AddEvent(event events.Event) {
	if s == nil {
		return
	}
	switch events.CanonicalKind(event.Kind) {
	case events.EventLLMCallCompleted:
	case events.EventLLMCallFailed:
		s.Failures++
		if adminBoolPayload(event.Payload, "retrying") {
			s.Retries++
		} else {
			s.FinalFailures++
		}
		return
	default:
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
	TraceKey                 string    `json:"trace_key"`
	ExecID                   string    `json:"exec_id,omitempty"`
	TraceID                  string    `json:"trace_id,omitempty"`
	SessionID                string    `json:"session_id,omitempty"`
	Events                   int       `json:"events"`
	LLMCalls                 int       `json:"llm_calls"`
	LLMFailures              int       `json:"llm_failures"`
	LLMRetries               int       `json:"llm_retries"`
	LLMFinalFailures         int       `json:"llm_final_failures"`
	ToolCalls                int       `json:"tool_calls"`
	ToolFailures             int       `json:"tool_failures"`
	TasksStarted             int       `json:"tasks_started"`
	TasksCompleted           int       `json:"tasks_completed"`
	TasksFailed              int       `json:"tasks_failed"`
	PermissionAllowed        int       `json:"permission_allowed"`
	PermissionDenied         int       `json:"permission_denied"`
	HookFailures             int       `json:"hook_failures"`
	InputTokens              int       `json:"input_tokens"`
	OutputTokens             int       `json:"output_tokens"`
	CostUSD                  float64   `json:"cost_usd"`
	LatencyMS                float64   `json:"average_latency_ms"`
	SchemaViolations         int       `json:"schema_violations"`
	SchemaValidationPassed   int       `json:"schema_validation_passed"`
	SchemaValidationFailed   int       `json:"schema_validation_failed"`
	SchemaRepairsStarted     int       `json:"schema_repairs_started"`
	SchemaRepairsCompleted   int       `json:"schema_repairs_completed"`
	SchemaRepairsFailed      int       `json:"schema_repairs_failed"`
	SchemaRepairs            int       `json:"schema_repairs"`
	SchemaRepairFailures     int       `json:"schema_repair_failures"`
	BudgetExceeded           int       `json:"budget_exceeded"`
	BudgetExceededTokens     int       `json:"budget_exceeded_tokens"`
	BudgetExceededCostUSD    int       `json:"budget_exceeded_cost_usd"`
	BudgetExceededWallClock  int       `json:"budget_exceeded_wallclock"`
	BudgetExceededIterations int       `json:"budget_exceeded_iterations"`
	SignatureDecisions       int       `json:"signature_decisions"`
	SignatureValid           int       `json:"signature_valid"`
	SignatureInvalid         int       `json:"signature_invalid"`
	SignatureMissing         int       `json:"signature_missing"`
	SignatureSecretMissing   int       `json:"signature_secret_missing"`
	IdempotencyDecisions     int       `json:"idempotency_decisions"`
	IdempotencyReserved      int       `json:"idempotency_reserved"`
	IdempotencyDuplicate     int       `json:"idempotency_duplicate"`
	IdempotencyRetry         int       `json:"idempotency_retry"`
	FirstEventID             uint64    `json:"first_event_id"`
	LastEventID              uint64    `json:"last_event_id"`
	LastKind                 string    `json:"last_kind,omitempty"`
	LastAt                   time.Time `json:"last_at,omitempty"`
	llmUsage                 adminLLMUsageSummary
	decisionMetrics          adminDecisionMetricsSummary
}

func (s *adminTraceSummary) AddEvent(event events.Event) {
	if s == nil {
		return
	}
	switch events.CanonicalKind(event.Kind) {
	case events.EventToolCallStarted:
		s.ToolCalls++
	case events.EventToolCallFailed:
		s.ToolFailures++
	case events.EventTaskStarted:
		s.TasksStarted++
	case events.EventTaskCompleted:
		s.TasksCompleted++
	case events.EventTaskFailed:
		s.TasksFailed++
	case events.EventPermissionDecision:
		if adminBoolPayload(event.Payload, "allowed") {
			s.PermissionAllowed++
		} else {
			s.PermissionDenied++
		}
	case events.EventHookFailed:
		s.HookFailures++
	case events.EventSignatureDecision, events.EventIdempotencyDecision:
		s.decisionMetrics.AddEvent(event)
		s.SignatureDecisions = s.decisionMetrics.SignatureDecisions
		s.SignatureValid = s.decisionMetrics.SignatureValid
		s.SignatureInvalid = s.decisionMetrics.SignatureInvalid
		s.SignatureMissing = s.decisionMetrics.SignatureMissing
		s.SignatureSecretMissing = s.decisionMetrics.SignatureSecretMissing
		s.IdempotencyDecisions = s.decisionMetrics.IdempotencyDecisions
		s.IdempotencyReserved = s.decisionMetrics.IdempotencyReserved
		s.IdempotencyDuplicate = s.decisionMetrics.IdempotencyDuplicate
		s.IdempotencyRetry = s.decisionMetrics.IdempotencyRetry
	case events.EventSchemaValidationPassed:
		s.SchemaValidationPassed++
	case events.EventSchemaValidationFailed:
		s.SchemaValidationFailed++
		s.SchemaViolations++
	case events.EventSchemaRepairStarted:
		s.SchemaRepairsStarted++
	case events.EventSchemaRepairCompleted:
		s.SchemaRepairsCompleted++
		s.SchemaRepairs++
	case events.EventSchemaRepairFailed:
		s.SchemaRepairsFailed++
		s.SchemaRepairFailures++
	case events.EventBudgetExceeded:
		s.BudgetExceeded++
		switch adminStringPayload(event.Payload, "budget") {
		case "tokens":
			s.BudgetExceededTokens++
		case "cost_usd":
			s.BudgetExceededCostUSD++
		case "wallclock":
			s.BudgetExceededWallClock++
		case "iterations":
			s.BudgetExceededIterations++
		}
	}
}

type adminTraceResponse struct {
	Status                 string                         `json:"status"`
	Execution              *adminExecutionResponse        `json:"execution,omitempty"`
	Events                 []adminEventResponse           `json:"events"`
	Sessions               int                            `json:"sessions"`
	SessionDetails         []adminSessionResponse         `json:"session_details,omitempty"`
	SchemaViolations       int                            `json:"schema_violations"`
	SchemaViolationDetails []adminSchemaViolationResponse `json:"schema_violation_details,omitempty"`
	LastEventID            uint64                         `json:"last_event_id"`
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

type adminTriggerResponse struct {
	Status    string `json:"status"`
	Output    string `json:"output,omitempty"`
	ExecID    string `json:"exec_id,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

func writeAdminTriggerStatus(w http.ResponseWriter, code int, status string, result planRunResult) {
	writeAdminTriggerOutput(w, code, status, result, "")
}

func writeAdminTriggerOutput(w http.ResponseWriter, code int, status string, result planRunResult, output string) {
	response := adminTriggerResponse{Status: status, Output: output}
	if result.HasSession {
		response.ExecID = result.Session.ExecID
		response.TraceID = result.Session.TraceID
		response.SessionID = result.Session.SessionID
	}
	writeJSON(w, code, response)
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

type adminSessionResponse struct {
	ExecID          string    `json:"exec_id"`
	SessionID       string    `json:"session_id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	TraceID         string    `json:"trace_id,omitempty"`
	Model           string    `json:"model,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	MaxIterations   int       `json:"max_iterations,omitempty"`
	MaxTokens       int       `json:"max_tokens,omitempty"`
	MaxCostUSD      float64   `json:"max_cost_usd,omitempty"`
	MaxWallClockMS  int64     `json:"max_wallclock_ms,omitempty"`
}

type adminSchemaViolationResponse struct {
	ID         uint64    `json:"id"`
	At         time.Time `json:"at"`
	ExecID     string    `json:"exec_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	SchemaName string    `json:"schema_name,omitempty"`
	Error      string    `json:"error,omitempty"`
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

func parseAdminTraceAfterID(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	afterID, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return afterID, nil
}

func summarizeAdminTraces(recorded []events.Event, limit int) []adminTraceSummary {
	byKey := make(map[string]*adminTraceSummary)
	order := make([]string, 0)
	for _, event := range recorded {
		key := adminTraceKey(event)
		summary, ok := byKey[key]
		if !ok {
			summary = &adminTraceSummary{
				TraceKey:     key,
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
		summary.AddEvent(event)
		summary.llmUsage.AddEvent(event)
		summary.LLMCalls = summary.llmUsage.Calls
		summary.LLMFailures = summary.llmUsage.Failures
		summary.LLMRetries = summary.llmUsage.Retries
		summary.LLMFinalFailures = summary.llmUsage.FinalFailures
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

func recentAdminExecutions(executions []state.Execution, limit int) []adminExecutionResponse {
	if limit <= 0 || len(executions) == 0 {
		return nil
	}
	sorted := append([]state.Execution(nil), executions...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].StartedAt.Equal(sorted[j].StartedAt) {
			return sorted[i].StartedAt.After(sorted[j].StartedAt)
		}
		return sorted[i].ExecID > sorted[j].ExecID
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	recent := make([]adminExecutionResponse, 0, len(sorted))
	for _, execution := range sorted {
		recent = append(recent, *adminExecutionResponseFromState(execution))
	}
	return recent
}

func adminSessionResponseFromRuntime(session runtimeplan.Session) adminSessionResponse {
	return adminSessionResponse{
		ExecID:          session.ExecID,
		SessionID:       session.SessionID,
		ParentSessionID: session.ParentSessionID,
		TraceID:         session.TraceID,
		Model:           session.Model,
		StartedAt:       session.StartedAt,
		MaxIterations:   session.Budget.MaxIterations,
		MaxTokens:       session.Budget.MaxTokens,
		MaxCostUSD:      session.Budget.MaxCostUSD,
		MaxWallClockMS:  session.Budget.MaxWallClock.Milliseconds(),
	}
}

func adminSchemaViolationResponseFromState(violation state.SchemaViolation) adminSchemaViolationResponse {
	return adminSchemaViolationResponse{
		ID:         violation.ID,
		At:         violation.At,
		ExecID:     violation.ExecID,
		SessionID:  violation.SessionID,
		SchemaName: violation.SchemaName,
		Error:      events.RedactText(violation.Error),
	}
}

func recentAdminSchemaViolations(violations []state.SchemaViolation, limit int) []adminSchemaViolationResponse {
	if limit <= 0 || len(violations) == 0 {
		return nil
	}
	sorted := append([]state.SchemaViolation(nil), violations...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].At.Equal(sorted[j].At) {
			return sorted[i].At.After(sorted[j].At)
		}
		return sorted[i].ID > sorted[j].ID
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	recent := make([]adminSchemaViolationResponse, 0, len(sorted))
	for _, violation := range sorted {
		recent = append(recent, adminSchemaViolationResponseFromState(violation))
	}
	return recent
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
