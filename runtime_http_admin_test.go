package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/events"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/state"
)

func TestHTTPAdminHealthAllowsDevAccessWithoutToken(t *testing.T) {
	t.Setenv("PIP_ENV", "dev")
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store, eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status      string `json:"status"`
		StateStore  bool   `json:"state_store"`
		EventStream bool   `json:"event_stream"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || !body.StateStore || !body.EventStream {
		t.Fatalf("body = %+v, want ok with store and stream", body)
	}
}

func TestHTTPAdminHealthIncludesRecentExecutionsFromStateStore(t *testing.T) {
	t.Setenv("PIP_ENV", "dev")
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      state.ExecutionCompleted,
		StartedAt:   now.Add(-3 * time.Minute),
		CompletedAt: now.Add(-2 * time.Minute),
	})
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_2",
		TraceID:   "trace_2",
		Status:    state.ExecutionRunning,
		StartedAt: now.Add(-2 * time.Minute),
	})
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_3",
		TraceID:     "trace_3",
		Status:      state.ExecutionFailed,
		StartedAt:   now.Add(-1 * time.Minute),
		CompletedAt: now,
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/health?last=2", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status           string `json:"status"`
		Executions       int    `json:"executions"`
		RecentExecutions []struct {
			ExecID  string `json:"exec_id"`
			TraceID string `json:"trace_id"`
			Status  string `json:"status"`
		} `json:"recent_executions"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Executions != 3 || len(body.RecentExecutions) != 2 {
		t.Fatalf("body = %+v, want two of three recent executions", body)
	}
	if body.RecentExecutions[0].ExecID != "exec_3" || body.RecentExecutions[0].Status != "failed" ||
		body.RecentExecutions[1].ExecID != "exec_2" || body.RecentExecutions[1].Status != "running" {
		t.Fatalf("recent executions = %+v, want newest executions first", body.RecentExecutions)
	}
}

func TestHTTPAdminRejectsMissingTokenOutsideDevMode(t *testing.T) {
	t.Setenv("PIP_ENV", "production")
	t.Setenv("PIP_ADMIN_TOKEN", "")
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/health", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	var body struct {
		Status string `json:"status"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "admin_token_required" {
		t.Fatalf("status body = %q, want admin_token_required", body.Status)
	}
}

func TestHTTPAdminRequiresBearerTokenWhenConfigured(t *testing.T) {
	handler := newTestAdminHTTPHandler(t, httpRuntime{adminToken: "secret-admin-token"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/health", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if strings.Contains(unauthorized.Body.String(), "secret-admin-token") {
		t.Fatalf("unauthorized response echoed admin token: %q", unauthorized.Body.String())
	}

	forbidden := httptest.NewRecorder()
	invalid := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	invalid.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(forbidden, invalid)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden status = %d, want %d", forbidden.Code, http.StatusForbidden)
	}
	if strings.Contains(forbidden.Body.String(), "secret-admin-token") || strings.Contains(forbidden.Body.String(), "wrong-token") {
		t.Fatalf("forbidden response echoed admin token: %q", forbidden.Body.String())
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.Code, http.StatusOK)
	}
}

func TestDefaultHTTPRuntimeLoadsAdminTokenFromEnv(t *testing.T) {
	t.Setenv("PIP_ADMIN_TOKEN", " env-admin-token ")
	handler := newTestAdminHTTPHandler(t, defaultHTTPRuntime())

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/admin/health", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	req.Header.Set("Authorization", "Bearer env-admin-token")
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.Code, http.StatusOK)
	}
}

func TestHTTPAdminStatusSummarizesStateStoreExecutions(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      state.ExecutionCompleted,
		StartedAt:   now,
		CompletedAt: now.Add(time.Second),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
	})
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_2",
		TraceID:   "trace_2",
		Status:    state.ExecutionRunning,
		StartedAt: now.Add(time.Minute),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_2",
		SessionID: "sess_2",
		TraceID:   "trace_2",
		Model:     "openai/gpt-4.1",
		StartedAt: now.Add(time.Minute),
	})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status     string         `json:"status"`
		Sessions   int            `json:"sessions"`
		Executions int            `json:"executions"`
		ByStatus   map[string]int `json:"by_status"`
		Events     int            `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Sessions != 2 || body.Executions != 2 || body.Events != 2 {
		t.Fatalf("body = %+v, want two sessions and executions", body)
	}
	if body.ByStatus["completed"] != 1 || body.ByStatus["running"] != 1 {
		t.Fatalf("by_status = %+v, want completed/running counts", body.ByStatus)
	}
}

func TestHTTPAdminStatusIncludesSchemaConformanceFromStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    state.ExecutionFailed,
		StartedAt: now,
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
	})
	addAdminSchemaViolation(t, store, state.SchemaViolation{
		ExecID:     "exec_1",
		SessionID:  "sess_1",
		SchemaName: "ovr.httpTestReply",
		Error:      "status must be string",
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaValidationFailed,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema": "ovr.httpTestReply",
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaRepairStarted,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema": "ovr.httpTestReply",
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaRepairCompleted,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema": "ovr.httpTestReply",
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaRepairFailed,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema": "ovr.httpTestReply",
			"error":  "repair still invalid",
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaValidationPassed,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema":   "ovr.httpTestReply",
			"repaired": true,
		},
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status                 string `json:"status"`
		SchemaViolations       int    `json:"schema_violations"`
		SchemaValidationPassed int    `json:"schema_validation_passed"`
		SchemaValidationFailed int    `json:"schema_validation_failed"`
		SchemaRepairsStarted   int    `json:"schema_repairs_started"`
		SchemaRepairsCompleted int    `json:"schema_repairs_completed"`
		SchemaRepairsFailed    int    `json:"schema_repairs_failed"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" {
		t.Fatalf("status body = %q, want ok", body.Status)
	}
	if body.SchemaViolations != 1 {
		t.Fatalf("schema_violations = %d, want 1", body.SchemaViolations)
	}
	if body.SchemaValidationPassed != 1 || body.SchemaValidationFailed != 1 {
		t.Fatalf("schema validation counts = passed %d failed %d, want 1/1", body.SchemaValidationPassed, body.SchemaValidationFailed)
	}
	if body.SchemaRepairsStarted != 1 || body.SchemaRepairsCompleted != 1 {
		t.Fatalf("schema repair counts = started %d completed %d, want 1/1", body.SchemaRepairsStarted, body.SchemaRepairsCompleted)
	}
	if body.SchemaRepairsFailed != 1 {
		t.Fatalf("schema_repairs_failed = %d, want 1", body.SchemaRepairsFailed)
	}
}

func TestHTTPAdminStatusIncludesRecentSchemaViolationsFromStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 24, 15, 0, 0, 0, time.UTC)
	addAdminSchemaViolation(t, store, state.SchemaViolation{
		At:         now.Add(-3 * time.Minute),
		ExecID:     "exec_1",
		SessionID:  "sess_1",
		SchemaName: "First",
		Error:      "old violation",
	})
	addAdminSchemaViolation(t, store, state.SchemaViolation{
		At:         now.Add(-2 * time.Minute),
		ExecID:     "exec_2",
		SessionID:  "sess_2",
		SchemaName: "Second",
		Error:      "middle token=middle-secret",
	})
	addAdminSchemaViolation(t, store, state.SchemaViolation{
		At:         now.Add(-1 * time.Minute),
		ExecID:     "exec_3",
		SessionID:  "sess_3",
		SchemaName: "Third",
		Error:      "newest api_key=newest-secret",
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status?last=2", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status                 string `json:"status"`
		SchemaViolations       int    `json:"schema_violations"`
		RecentSchemaViolations []struct {
			ExecID     string `json:"exec_id"`
			SessionID  string `json:"session_id"`
			SchemaName string `json:"schema_name"`
			Error      string `json:"error"`
		} `json:"recent_schema_violations"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.SchemaViolations != 3 || len(body.RecentSchemaViolations) != 2 {
		t.Fatalf("body = %+v, want two of three recent schema violations", body)
	}
	if body.RecentSchemaViolations[0].ExecID != "exec_3" || body.RecentSchemaViolations[0].SchemaName != "Third" ||
		body.RecentSchemaViolations[0].Error != "newest api_key=[REDACTED]" {
		t.Fatalf("newest violation = %+v, want redacted exec_3 violation", body.RecentSchemaViolations[0])
	}
	if body.RecentSchemaViolations[1].ExecID != "exec_2" || body.RecentSchemaViolations[1].SchemaName != "Second" ||
		body.RecentSchemaViolations[1].Error != "middle token=[REDACTED]" {
		t.Fatalf("second newest violation = %+v, want redacted exec_2 violation", body.RecentSchemaViolations[1])
	}
}

func TestHTTPAdminStatusIncludesLLMUsageFromEvents(t *testing.T) {
	store := state.NewMemoryStore()
	addAdminStoreEvent(t, store, events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_1",
		Payload: map[string]any{
			"input_tokens":  11,
			"output_tokens": 7,
			"cost_usd":      0.015,
			"latency_ms":    int64(25),
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_2",
		Payload: map[string]any{
			"input_tokens":  5,
			"output_tokens": 3,
			"cost_usd":      0.005,
			"latency_ms":    35,
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:   events.EventLLMCallStarted,
		ExecID: "exec_3",
		Payload: map[string]any{
			"input_tokens": 99,
			"cost_usd":     9.9,
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:   events.EventLLMCallFailed,
		ExecID: "exec_4",
		Payload: map[string]any{
			"retrying": true,
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:   events.EventLLMCallFailed,
		ExecID: "exec_5",
		Payload: map[string]any{
			"retrying": false,
		},
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status           string  `json:"status"`
		LLMCalls         int     `json:"llm_calls"`
		LLMFailures      int     `json:"llm_failures"`
		LLMRetries       int     `json:"llm_retries"`
		LLMFinalFailures int     `json:"llm_final_failures"`
		InputTokens      int     `json:"input_tokens"`
		OutputTokens     int     `json:"output_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		AverageLatencyMS float64 `json:"average_latency_ms"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" {
		t.Fatalf("status body = %q, want ok", body.Status)
	}
	if body.LLMCalls != 2 || body.InputTokens != 16 || body.OutputTokens != 10 {
		t.Fatalf("usage body = %+v, want two completed LLM calls with 16/10 tokens", body)
	}
	if body.LLMFailures != 2 || body.LLMRetries != 1 || body.LLMFinalFailures != 1 {
		t.Fatalf("LLM failure metrics = %+v, want failures/retries/final failures", body)
	}
	if body.CostUSD < 0.0199 || body.CostUSD > 0.0201 {
		t.Fatalf("cost_usd = %v, want 0.02", body.CostUSD)
	}
	if body.AverageLatencyMS != 30 {
		t.Fatalf("average_latency_ms = %v, want 30", body.AverageLatencyMS)
	}
}

func TestHTTPAdminStatusIncludesHarnessMetricsFromEvents(t *testing.T) {
	store := state.NewMemoryStore()
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionSaved, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"status": "completed",
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionCancelled, ExecID: "exec_2", SessionID: "sess_2"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventToolCallStarted, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventToolCallCompleted, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventToolCallFailed, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventTaskStarted, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventTaskCompleted, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventTaskFailed, ExecID: "exec_1", SessionID: "sess_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventPermissionDecision, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"allowed": true,
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventPermissionDecision, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"allowed": false,
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventBudgetExceeded, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"budget": "tokens",
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventBudgetExceeded, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"budget": "cost_usd",
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventBudgetExceeded, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"budget": "wallclock",
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventBudgetExceeded, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"budget": "iterations",
	}})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventHookFailed, ExecID: "exec_1", SessionID: "sess_1", Payload: map[string]any{
		"blocked_kind": string(events.EventLLMCallCompleted),
		"error":        "hook llm_call_completed blocked event: audit denied",
	}})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status                 string `json:"status"`
		SessionsStarted        int    `json:"sessions_started"`
		SessionsCompleted      int    `json:"sessions_completed"`
		SessionsCancelled      int    `json:"sessions_cancelled"`
		ToolCalls              int    `json:"tool_calls"`
		ToolCallsCompleted     int    `json:"tool_calls_completed"`
		ToolFailures           int    `json:"tool_failures"`
		TasksStarted           int    `json:"tasks_started"`
		TasksCompleted         int    `json:"tasks_completed"`
		TasksFailed            int    `json:"tasks_failed"`
		PermissionAllowed      int    `json:"permission_allowed"`
		PermissionDenied       int    `json:"permission_denied"`
		BudgetExceeded         int    `json:"budget_exceeded"`
		BudgetExceededTokens   int    `json:"budget_exceeded_tokens"`
		BudgetExceededCostUSD  int    `json:"budget_exceeded_cost_usd"`
		BudgetExceededWalltime int    `json:"budget_exceeded_wallclock"`
		BudgetExceededIter     int    `json:"budget_exceeded_iterations"`
		HookFailures           int    `json:"hook_failures"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" {
		t.Fatalf("status body = %q, want ok", body.Status)
	}
	if body.SessionsStarted != 1 || body.SessionsCompleted != 1 || body.SessionsCancelled != 1 {
		t.Fatalf("session metrics = %+v, want started/completed/cancelled counts", body)
	}
	if body.ToolCalls != 1 || body.ToolCallsCompleted != 1 || body.ToolFailures != 1 {
		t.Fatalf("tool metrics = %+v, want started/completed/failed counts", body)
	}
	if body.TasksStarted != 1 || body.TasksCompleted != 1 || body.TasksFailed != 1 {
		t.Fatalf("task metrics = %+v, want started/completed/failed counts", body)
	}
	if body.PermissionAllowed != 1 || body.PermissionDenied != 1 {
		t.Fatalf("permission metrics = %+v, want allowed and denied counts", body)
	}
	if body.BudgetExceeded != 4 || body.BudgetExceededTokens != 1 || body.BudgetExceededCostUSD != 1 ||
		body.BudgetExceededWalltime != 1 || body.BudgetExceededIter != 1 {
		t.Fatalf("budget metrics = %+v, want per-budget exceeded counts", body)
	}
	if body.HookFailures != 1 {
		t.Fatalf("hook_failures = %d, want 1", body.HookFailures)
	}
}

func TestHTTPAdminStatusIncludesTriggerDecisionMetricsFromEvents(t *testing.T) {
	store := state.NewMemoryStore()
	for _, decision := range []string{"valid", "invalid", "missing", "secret_missing"} {
		addAdminStoreEvent(t, store, events.Event{Kind: events.EventSignatureDecision, Payload: map[string]any{
			"decision": decision,
		}})
	}
	for _, decision := range []string{"reserved", "duplicate", "retry"} {
		addAdminStoreEvent(t, store, events.Event{Kind: events.EventIdempotencyDecision, ExecID: "exec_1", Payload: map[string]any{
			"decision": decision,
		}})
	}
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status                 string `json:"status"`
		SignatureDecisions     int    `json:"signature_decisions"`
		SignatureValid         int    `json:"signature_valid"`
		SignatureInvalid       int    `json:"signature_invalid"`
		SignatureMissing       int    `json:"signature_missing"`
		SignatureSecretMissing int    `json:"signature_secret_missing"`
		IdempotencyDecisions   int    `json:"idempotency_decisions"`
		IdempotencyReserved    int    `json:"idempotency_reserved"`
		IdempotencyDuplicate   int    `json:"idempotency_duplicate"`
		IdempotencyRetry       int    `json:"idempotency_retry"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" {
		t.Fatalf("status body = %q, want ok", body.Status)
	}
	if body.SignatureDecisions != 4 || body.SignatureValid != 1 || body.SignatureInvalid != 1 ||
		body.SignatureMissing != 1 || body.SignatureSecretMissing != 1 {
		t.Fatalf("signature metrics = %+v, want per-decision counts", body)
	}
	if body.IdempotencyDecisions != 3 || body.IdempotencyReserved != 1 ||
		body.IdempotencyDuplicate != 1 || body.IdempotencyRetry != 1 {
		t.Fatalf("idempotency metrics = %+v, want per-decision counts", body)
	}
}

func TestHTTPAdminTracesListsRecentTraceSummariesFromEventStream(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallFailed, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2", Payload: map[string]any{
		"retrying": true,
	}})
	appendAdminEvent(t, stream, events.Event{
		Kind:      events.EventLLMCallCompleted,
		ExecID:    "exec_2",
		SessionID: "sess_2",
		TraceID:   "trace_2",
		Payload: map[string]any{
			"input_tokens":  13,
			"output_tokens": 8,
			"cost_usd":      0.021,
			"latency_ms":    41,
		},
	})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventToolCallStarted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSchemaValidationFailed, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSchemaRepairCompleted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventTaskStarted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventTaskCompleted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventTaskFailed, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventHookFailed, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventBudgetExceeded, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces?last=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			ExecID           string  `json:"exec_id"`
			TraceID          string  `json:"trace_id"`
			Events           int     `json:"events"`
			LLMCalls         int     `json:"llm_calls"`
			LLMFailures      int     `json:"llm_failures"`
			LLMRetries       int     `json:"llm_retries"`
			LLMFinalFailures int     `json:"llm_final_failures"`
			ToolCalls        int     `json:"tool_calls"`
			TasksStarted     int     `json:"tasks_started"`
			TasksCompleted   int     `json:"tasks_completed"`
			TasksFailed      int     `json:"tasks_failed"`
			HookFailures     int     `json:"hook_failures"`
			SchemaViolations int     `json:"schema_violations"`
			SchemaRepairs    int     `json:"schema_repairs"`
			BudgetExceeded   int     `json:"budget_exceeded"`
			InputTokens      int     `json:"input_tokens"`
			OutputTokens     int     `json:"output_tokens"`
			CostUSD          float64 `json:"cost_usd"`
			AverageLatencyMS float64 `json:"average_latency_ms"`
			LastEventID      uint64  `json:"last_event_id"`
			LastKind         string  `json:"last_kind"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one trace", body)
	}
	if body.Traces[0].ExecID != "exec_2" || body.Traces[0].TraceID != "trace_2" || body.Traces[0].Events != 11 {
		t.Fatalf("trace = %+v, want exec_2 summary", body.Traces[0])
	}
	if body.Traces[0].LastEventID != 12 || body.Traces[0].LastKind != string(events.EventBudgetExceeded) {
		t.Fatalf("trace last event = %+v, want event 12 budget exceeded", body.Traces[0])
	}
	if body.Traces[0].LLMCalls != 1 || body.Traces[0].InputTokens != 13 || body.Traces[0].OutputTokens != 8 ||
		body.Traces[0].CostUSD < 0.0209 || body.Traces[0].CostUSD > 0.0211 || body.Traces[0].AverageLatencyMS != 41 {
		t.Fatalf("trace LLM usage = %+v, want completed LLM metrics", body.Traces[0])
	}
	if body.Traces[0].LLMFailures != 1 || body.Traces[0].LLMRetries != 1 || body.Traces[0].LLMFinalFailures != 0 {
		t.Fatalf("trace LLM failure metrics = %+v, want retrying failure count", body.Traces[0])
	}
	if body.Traces[0].ToolCalls != 1 || body.Traces[0].SchemaViolations != 1 ||
		body.Traces[0].SchemaRepairs != 1 || body.Traces[0].BudgetExceeded != 1 {
		t.Fatalf("trace event counts = %+v, want tool/schema/budget counters", body.Traces[0])
	}
	if body.Traces[0].TasksStarted != 1 || body.Traces[0].TasksCompleted != 1 || body.Traces[0].TasksFailed != 1 {
		t.Fatalf("trace task counts = %+v, want task counters", body.Traces[0])
	}
	if body.Traces[0].HookFailures != 1 {
		t.Fatalf("trace hook failures = %+v, want one hook failure", body.Traces[0])
	}
}

func TestHTTPAdminTracesListsPersistentTraceSummariesFromStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventLLMCallCompleted, ExecID: "exec_2", SessionID: "sess_2", TraceID: "trace_2"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces?last=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			ExecID      string `json:"exec_id"`
			TraceID     string `json:"trace_id"`
			Events      int    `json:"events"`
			LastEventID uint64 `json:"last_event_id"`
			LastKind    string `json:"last_kind"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one trace", body)
	}
	if body.Traces[0].ExecID != "exec_2" || body.Traces[0].TraceID != "trace_2" || body.Traces[0].Events != 2 {
		t.Fatalf("trace = %+v, want exec_2 summary", body.Traces[0])
	}
	if body.Traces[0].LastEventID != 3 || body.Traces[0].LastKind != string(events.EventLLMCallCompleted) {
		t.Fatalf("trace last event = %+v, want event 3 llm completion", body.Traces[0])
	}
}

func TestHTTPAdminTracesSummarizesPermissionDecisions(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{Kind: events.EventPermissionDecision, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1", Payload: map[string]any{
		"allowed": true,
	}})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventPermissionDecision, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1", Payload: map[string]any{
		"allowed": false,
	}})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			ExecID            string `json:"exec_id"`
			PermissionAllowed int    `json:"permission_allowed"`
			PermissionDenied  int    `json:"permission_denied"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one trace", body)
	}
	if body.Traces[0].ExecID != "exec_1" || body.Traces[0].PermissionAllowed != 1 || body.Traces[0].PermissionDenied != 1 {
		t.Fatalf("trace = %+v, want allowed and denied permission counts", body.Traces[0])
	}
}

func TestHTTPAdminTracesSummarizesTriggerDecisions(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSignatureDecision, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1", Payload: map[string]any{
		"decision": "valid",
	}})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSignatureDecision, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1", Payload: map[string]any{
		"decision": "invalid",
	}})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventIdempotencyDecision, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1", Payload: map[string]any{
		"decision": "reserved",
	}})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventIdempotencyDecision, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1", Payload: map[string]any{
		"decision": "duplicate",
	}})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			ExecID               string `json:"exec_id"`
			SignatureDecisions   int    `json:"signature_decisions"`
			SignatureValid       int    `json:"signature_valid"`
			SignatureInvalid     int    `json:"signature_invalid"`
			IdempotencyDecisions int    `json:"idempotency_decisions"`
			IdempotencyReserved  int    `json:"idempotency_reserved"`
			IdempotencyDuplicate int    `json:"idempotency_duplicate"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one trace", body)
	}
	if body.Traces[0].ExecID != "exec_1" ||
		body.Traces[0].SignatureDecisions != 2 || body.Traces[0].SignatureValid != 1 || body.Traces[0].SignatureInvalid != 1 ||
		body.Traces[0].IdempotencyDecisions != 2 || body.Traces[0].IdempotencyReserved != 1 || body.Traces[0].IdempotencyDuplicate != 1 {
		t.Fatalf("trace = %+v, want signature and idempotency decision counts", body.Traces[0])
	}
}

func TestHTTPAdminTracesIncludesSelectableKeyForEventsWithoutExecution(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{
		Kind: events.EventSignatureDecision,
		Payload: map[string]any{
			"decision": "missing",
		},
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			TraceKey string `json:"trace_key"`
			ExecID   string `json:"exec_id"`
			Events   int    `json:"events"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one runtime event trace", body)
	}
	if body.Traces[0].TraceKey != "event:1" || body.Traces[0].ExecID != "" || body.Traces[0].Events != 1 {
		t.Fatalf("trace = %+v, want selectable event key without exec id", body.Traces[0])
	}
}

func TestHTTPAdminTraceByKeyReturnsRuntimeEventWithoutExecution(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSignatureDecision, Payload: map[string]any{"decision": "missing"}})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/event:1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status      string `json:"status"`
		LastEventID uint64 `json:"last_event_id"`
		Events      []struct {
			ID     uint64 `json:"id"`
			ExecID string `json:"exec_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Events) != 1 {
		t.Fatalf("body = %+v, want one runtime event", body)
	}
	if body.Events[0].ID != 1 || body.Events[0].ExecID != "" || body.Events[0].Kind != string(events.EventSignatureDecision) {
		t.Fatalf("events = %+v, want signature event without exec id", body.Events)
	}
	if body.LastEventID != 1 {
		t.Fatalf("last_event_id = %d, want 1", body.LastEventID)
	}
}

func TestHTTPAdminTraceByExecutionIncludesExecutionAndEvents(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      state.ExecutionFailed,
		StartedAt:   now,
		CompletedAt: now.Add(time.Second),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
		Budget: runtimecore.Budget{
			MaxIterations: 8,
			MaxTokens:     1200,
			MaxCostUSD:    0.25,
			MaxWallClock:  2 * time.Second,
		},
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:          "exec_1",
		SessionID:       "sess_child",
		ParentSessionID: "sess_1",
		TraceID:         "trace_1",
		Model:           "anthropic/claude-haiku-4-5",
		StartedAt:       now.Add(500 * time.Millisecond),
	})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventPipeFailed, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_other", SessionID: "sess_other", TraceID: "trace_other"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store, eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status    string `json:"status"`
		Execution struct {
			ExecID  string `json:"exec_id"`
			TraceID string `json:"trace_id"`
			Status  string `json:"status"`
		} `json:"execution"`
		Events []struct {
			ExecID string `json:"exec_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
		Sessions       int `json:"sessions"`
		SessionDetails []struct {
			ExecID          string  `json:"exec_id"`
			SessionID       string  `json:"session_id"`
			ParentSessionID string  `json:"parent_session_id"`
			TraceID         string  `json:"trace_id"`
			Model           string  `json:"model"`
			MaxIterations   int     `json:"max_iterations"`
			MaxTokens       int     `json:"max_tokens"`
			MaxCostUSD      float64 `json:"max_cost_usd"`
			MaxWallClockMS  int64   `json:"max_wallclock_ms"`
		} `json:"session_details"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Execution.ExecID != "exec_1" || body.Execution.Status != "failed" {
		t.Fatalf("body = %+v, want failed exec_1", body)
	}
	if len(body.Events) != 2 || body.Events[0].ExecID != "exec_1" || body.Events[1].Kind != string(events.EventPipeFailed) {
		t.Fatalf("events = %+v, want only exec_1 events", body.Events)
	}
	if body.Sessions != 2 || len(body.SessionDetails) != 2 {
		t.Fatalf("sessions = %d details=%+v, want two session details", body.Sessions, body.SessionDetails)
	}
	if body.SessionDetails[0].SessionID != "sess_1" || body.SessionDetails[0].ParentSessionID != "" {
		t.Fatalf("root session details = %+v, want sess_1 without parent", body.SessionDetails[0])
	}
	if body.SessionDetails[0].MaxIterations != 8 || body.SessionDetails[0].MaxTokens != 1200 ||
		body.SessionDetails[0].MaxCostUSD != 0.25 || body.SessionDetails[0].MaxWallClockMS != 2000 {
		t.Fatalf("root session budget = %+v, want budget fields", body.SessionDetails[0])
	}
	if body.SessionDetails[1].SessionID != "sess_child" || body.SessionDetails[1].ParentSessionID != "sess_1" {
		t.Fatalf("child session details = %+v, want child lineage", body.SessionDetails[1])
	}
}

func TestHTTPAdminTraceByExecutionReadsPersistentEventsFromStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      state.ExecutionFailed,
		StartedAt:   now,
		CompletedAt: now.Add(time.Second),
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
	})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventPipeFailed, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_other", SessionID: "sess_other", TraceID: "trace_other"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Events []struct {
			ExecID string `json:"exec_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
		Sessions int `json:"sessions"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Events) != 2 {
		t.Fatalf("body = %+v, want two exec_1 events", body)
	}
	if body.Events[0].ExecID != "exec_1" || body.Events[1].Kind != string(events.EventPipeFailed) {
		t.Fatalf("events = %+v, want only exec_1 events", body.Events)
	}
	if body.Sessions != 1 {
		t.Fatalf("sessions = %d, want 1", body.Sessions)
	}
}

func TestHTTPAdminTraceByExecutionSupportsAfterIDFromStateStore(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    state.ExecutionRunning,
		StartedAt: now,
	})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_other", SessionID: "sess_other", TraceID: "trace_other"})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventLLMCallCompleted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1?after_id=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status      string `json:"status"`
		LastEventID uint64 `json:"last_event_id"`
		Events      []struct {
			ID     uint64 `json:"id"`
			ExecID string `json:"exec_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Events) != 2 {
		t.Fatalf("body = %+v, want two exec_1 events after id 1", body)
	}
	if body.Events[0].ID != 2 || body.Events[0].Kind != string(events.EventLLMCallStarted) ||
		body.Events[1].ID != 4 || body.Events[1].Kind != string(events.EventLLMCallCompleted) {
		t.Fatalf("events = %+v, want exec_1 events 2 and 4", body.Events)
	}
	if body.LastEventID != 4 {
		t.Fatalf("last_event_id = %d, want 4", body.LastEventID)
	}
}

func TestHTTPAdminTraceByExecutionSupportsAfterIDFromEventStream(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallStarted, ExecID: "exec_other", SessionID: "sess_other", TraceID: "trace_other"})
	appendAdminEvent(t, stream, events.Event{Kind: events.EventLLMCallCompleted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1?after_id=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status      string `json:"status"`
		LastEventID uint64 `json:"last_event_id"`
		Events      []struct {
			ID     uint64 `json:"id"`
			ExecID string `json:"exec_id"`
			Kind   string `json:"kind"`
		} `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Events) != 2 {
		t.Fatalf("body = %+v, want two exec_1 events after id 1", body)
	}
	if body.Events[0].ID != 2 || body.Events[1].ID != 4 || body.LastEventID != 4 {
		t.Fatalf("body = %+v, want event IDs 2 and 4 with last_event_id 4", body)
	}
}

func TestHTTPAdminTraceByExecutionAfterIDReturnsOKWithNoNewEventsForKnownExecution(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    state.ExecutionRunning,
		StartedAt: now,
	})
	addAdminStoreEvent(t, store, events.Event{Kind: events.EventSessionStarted, ExecID: "exec_1", SessionID: "sess_1", TraceID: "trace_1"})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1?after_id=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status      string `json:"status"`
		LastEventID uint64 `json:"last_event_id"`
		Events      []struct {
			ID uint64 `json:"id"`
		} `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Events) != 0 || body.LastEventID != 1 {
		t.Fatalf("body = %+v, want ok with no new events and last_event_id 1", body)
	}
}

func TestHTTPAdminTraceByExecutionIncludesPersistentSchemaConformance(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	saveAdminExecution(t, store, state.Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    state.ExecutionCompleted,
		StartedAt: now,
	})
	saveAdminSession(t, store, runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
		StartedAt: now,
	})
	addAdminSchemaViolation(t, store, state.SchemaViolation{
		At:         now.Add(250 * time.Millisecond),
		ExecID:     "exec_1",
		SessionID:  "sess_1",
		SchemaName: "ovr.httpTestReply",
		Error:      "status must be string token=super-secret",
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaValidationFailed,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema": "ovr.httpTestReply",
			"error":  "status must be string",
		},
	})
	addAdminStoreEvent(t, store, events.Event{
		Kind:      events.EventSchemaRepairCompleted,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"schema": "ovr.httpTestReply",
		},
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status           string `json:"status"`
		SchemaViolations int    `json:"schema_violations"`
		ViolationDetails []struct {
			ID         uint64    `json:"id"`
			At         time.Time `json:"at"`
			ExecID     string    `json:"exec_id"`
			SessionID  string    `json:"session_id"`
			SchemaName string    `json:"schema_name"`
			Error      string    `json:"error"`
		} `json:"schema_violation_details"`
		Events []struct {
			Kind    string         `json:"kind"`
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.SchemaViolations != 1 {
		t.Fatalf("body = %+v, want ok with one schema violation", body)
	}
	if len(body.ViolationDetails) != 1 {
		t.Fatalf("schema violation details = %+v, want one detail", body.ViolationDetails)
	}
	violation := body.ViolationDetails[0]
	if violation.ID == 0 || violation.At.IsZero() || violation.ExecID != "exec_1" ||
		violation.SessionID != "sess_1" || violation.SchemaName != "ovr.httpTestReply" {
		t.Fatalf("schema violation detail = %+v, want stored violation metadata", violation)
	}
	if violation.Error != "status must be string token=[REDACTED]" {
		t.Fatalf("schema violation error = %q, want redacted token", violation.Error)
	}
	if len(body.Events) != 2 {
		t.Fatalf("events = %d, want schema validation and repair events", len(body.Events))
	}
	if body.Events[0].Kind != string(events.EventSchemaValidationFailed) || body.Events[1].Kind != string(events.EventSchemaRepairCompleted) {
		t.Fatalf("events = %+v, want schema validation failed then repair completed", body.Events)
	}
	if body.Events[0].Payload["schema"] != "ovr.httpTestReply" || body.Events[1].Payload["schema"] != "ovr.httpTestReply" {
		t.Fatalf("event payloads = %+v, want schema names visible", body.Events)
	}
}

func TestHTTPAdminTraceByExecutionIncludesRedactedEventPayload(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	appendAdminEvent(t, stream, events.Event{
		Kind:      events.EventBeforeTool,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"tool":          "load_ticket",
			"authorization": "Bearer root-token",
			"nested": map[string]any{
				"api_key": "nested-api-key",
				"safe":    "visible",
			},
		},
	})
	handler := newTestAdminHTTPHandler(t, httpRuntime{eventStream: stream})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces/exec_1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
		Events []struct {
			ExecID  string         `json:"exec_id"`
			Payload map[string]any `json:"payload"`
		} `json:"events"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Events) != 1 || body.Events[0].ExecID != "exec_1" {
		t.Fatalf("body = %+v, want one exec_1 event", body)
	}
	if body.Events[0].Payload["authorization"] != "[REDACTED]" {
		t.Fatalf("authorization payload = %v, want [REDACTED]", body.Events[0].Payload["authorization"])
	}
	nested := body.Events[0].Payload["nested"].(map[string]any)
	if nested["api_key"] != "[REDACTED]" {
		t.Fatalf("nested api_key payload = %v, want [REDACTED]", nested["api_key"])
	}
	if body.Events[0].Payload["tool"] != "load_ticket" || nested["safe"] != "visible" {
		t.Fatalf("payload = %+v, want non-sensitive fields preserved", body.Events[0].Payload)
	}
}

func TestHTTPDevTraceViewerIsUnavailableOutsideDevMode(t *testing.T) {
	t.Setenv("PIP_ENV", "production")
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{adminToken: "secret-admin-token"})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if strings.Contains(rec.Body.String(), "secret-admin-token") {
		t.Fatalf("response leaked admin token: %q", rec.Body.String())
	}
}

func TestHTTPDevTraceViewerServesSelfContainedUIInDevMode(t *testing.T) {
	t.Setenv("PIP_ENV", "dev")
	handler := newTestAdminHTTPHandler(t, httpRuntime{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dev", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", contentType)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Ouvrier Dev Trace Viewer",
		"/admin/status",
		"/admin/traces",
		"/admin/traces/",
		"trace_key",
		"after_id",
		"last_event_id",
		"session_details",
		"schema_violations",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/dev body does not contain %q", want)
		}
	}
}

func TestHTTPDevTraceViewerRequiresConfiguredAdminToken(t *testing.T) {
	t.Setenv("PIP_ENV", "dev")
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{adminToken: "secret-admin-token"})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/dev", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dev", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want %d", authorized.Code, http.StatusOK)
	}
	if strings.Contains(authorized.Body.String(), "secret-admin-token") {
		t.Fatalf("/dev body leaked admin token: %q", authorized.Body.String())
	}
}

func newTestAdminHTTPHandler(t *testing.T, rt httpRuntime) http.Handler {
	t.Helper()
	if strings.TrimSpace(rt.adminToken) == "" {
		t.Setenv("PIP_ENV", "dev")
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	return handler
}

func decodeAdminJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
}

func saveAdminExecution(t *testing.T, store state.Store, execution state.Execution) {
	t.Helper()
	if err := store.SaveExecution(context.Background(), execution); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
}

func saveAdminSession(t *testing.T, store state.Store, session runtimecore.Session) {
	t.Helper()
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("SaveSession returned error: %v", err)
	}
}

func appendAdminEvent(t *testing.T, stream *events.EventStream, event events.Event) {
	t.Helper()
	if _, err := stream.Append(context.Background(), event); err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
}

func addAdminStoreEvent(t *testing.T, store state.Store, event events.Event) {
	t.Helper()
	if _, err := store.AddEvent(context.Background(), event); err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
}

func addAdminSchemaViolation(t *testing.T, store state.Store, violation state.SchemaViolation) {
	t.Helper()
	if _, err := store.AddSchemaViolation(context.Background(), violation); err != nil {
		t.Fatalf("AddSchemaViolation returned error: %v", err)
	}
}
