package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestHTTPAdminTriggerRequiresBearerTokenWhenConfigured(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for unauthorized admin trigger", len(scripted.requests))
	}
}

func TestHTTPAdminTriggerRunsExistingHTTPRouteThroughHarness(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body adminTriggerResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Output != `{"status":"classified"}` {
		t.Fatalf("body = %+v, want ok classified output", body)
	}
	if body.ExecID == "" || body.TraceID == "" || body.SessionID == "" {
		t.Fatalf("body = %+v, want admin trigger execution identifiers", body)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].Messages[0].Text() != `{"title":"broken"}` {
		t.Fatalf("provider input = %q, want trigger body JSON", scripted.requests[0].Messages[0].Text())
	}
}

func TestHTTPAdminTriggerWritesPipelineOutputToFileSink(t *testing.T) {
	outputRoot := t.TempDir()
	outputPath := filepath.Join(outputRoot, "admin-trigger-output.json")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File(outputPath)),
	}, httpRuntime{
		adminToken:   "secret-admin-token",
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("file"),
		sandbox:      fileSinkSandbox(t, outputRoot),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", outputPath, err)
	}
	if got := strings.TrimSpace(string(data)); got != `{"status":"classified"}` {
		t.Fatalf("file output = %q, want provider output", got)
	}
}

func TestHTTPAdminTriggerPushesPipelineOutputToWebhook(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{
		adminToken:   "secret-admin-token",
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("webhook"),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"status":"classified"}`)
}

func TestHTTPAdminTriggerPushesPipelineOutputToQueue(t *testing.T) {
	queueURI, publishes := newNATSPublishRecorder(t, "tickets.classified")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Queue(queueURI)),
	}, httpRuntime{
		adminToken:   "secret-admin-token",
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("queue"),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/tickets", `{"title":"broken"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	publish := assertNATSPublish(t, publishes)
	if publish.Subject != "tickets.classified" || publish.Payload != `{"status":"classified"}` {
		t.Fatalf("publish = %+v, want classified ticket payload", publish)
	}
}

func TestHTTPAdminTriggerPushesDirectInputToWebhook(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{adminToken: "secret-admin-token", toolExecutor: outputAllowedExecutor("webhook")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/events", `{"event":"created"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"event":"created"}`)
}

func TestHTTPAdminTriggerPushesDirectInputToQueue(t *testing.T) {
	queueURI, publishes := newNATSPublishRecorder(t, "events.created")
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Push(Queue(queueURI)),
	}, httpRuntime{adminToken: "secret-admin-token", toolExecutor: outputAllowedExecutor("queue")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/events", `{"event":"created"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	publish := assertNATSPublish(t, publishes)
	if publish.Subject != "events.created" || publish.Payload != `{"event":"created"}` {
		t.Fatalf("publish = %+v, want direct event payload", publish)
	}
}

func TestHTTPAdminTriggerWritesDirectInputToFileSink(t *testing.T) {
	outputRoot := t.TempDir()
	outputPath := filepath.Join(outputRoot, "admin-trigger-input.json")
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Sink(File(outputPath)),
	}, httpRuntime{adminToken: "secret-admin-token", toolExecutor: outputAllowedExecutor("file"), sandbox: fileSinkSandbox(t, outputRoot)})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/events", `{"event":"created"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", outputPath, err)
	}
	if got := strings.TrimSpace(string(data)); got != `{"event":"created"}` {
		t.Fatalf("file output = %q, want trigger body", got)
	}
}

func TestHTTPAdminTriggerLogsDirectSinkInputToEventStream(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Sink(Log()),
	}, httpRuntime{adminToken: "secret-admin-token", stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/events", `{"event":"created"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	event := assertSinkLoggedEvent(t, stream, "input", `{"event":"created"}`)
	if event.ExecID == "" || event.SessionID == "" || event.TraceID == "" {
		t.Fatalf("sink_logged event = %+v, want admin terminal-only execution identifiers", event)
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventPipelineStarted); !ok {
		t.Fatalf("events = %+v, want admin terminal-only pipeline started event", stream.List())
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventPipelineCompleted); !ok {
		t.Fatalf("events = %+v, want admin terminal-only pipeline completed event", stream.List())
	}
	executions, err := store.Executions(context.Background())
	if err != nil {
		t.Fatalf("Executions returned error: %v", err)
	}
	if len(executions) != 1 || executions[0].Status != state.ExecutionCompleted {
		t.Fatalf("executions = %+v, want completed admin terminal-only execution", executions)
	}
}

func TestHTTPAdminTriggerRunsParameterizedHTTPRouteThroughHarness(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestParameterizedAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "secret-admin-token", `{
		"method": "POST",
		"path": "/tickets/T-123",
		"body": {"title": "broken"}
	}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || body.Output != `{"status":"classified"}` {
		t.Fatalf("body = %+v, want ok classified output", body)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "body", `{"title":"broken"}`)
	assertRawJSONField(t, input, "path_params", `{"id":"T-123"}`)
}

func TestHTTPAdminTriggerMissingHTTPRouteReturnsNotFound(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "secret-admin-token", "POST", "/missing", `{"title":"broken"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "not_found" {
		t.Fatalf("status body = %q, want not_found", body.Status)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for missing route", len(scripted.requests))
	}
}

func TestHTTPAdminTriggerParameterizedHTTPRouteConcretePathMismatchReturnsNotFound(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestParameterizedAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "secret-admin-token", `{
		"method": "POST",
		"path": "/projects/T-123",
		"body": {"title": "broken"}
	}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "not_found" {
		t.Fatalf("status body = %q, want not_found", body.Status)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for missing concrete route", len(scripted.requests))
	}
}

func TestHTTPAdminTriggerParameterizedHTTPRouteExtraSegmentReturnsNotFound(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler := newTestParameterizedAdminTriggerHTTPHandler(t, httpRuntime{
		adminToken: "secret-admin-token",
		provider:   scripted,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "secret-admin-token", `{
		"method": "POST",
		"path": "/tickets/T-123/comments",
		"body": {"title": "broken"}
	}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if len(scripted.requests) != 0 {
		t.Fatalf("provider calls = %d, want 0 for concrete path with extra segment", len(scripted.requests))
	}
}

func TestHTTPAdminTriggerRejectsInvalidPayload(t *testing.T) {
	handler := newTestAdminTriggerHTTPHandler(t, httpRuntime{})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/trigger", strings.NewReader(`{"method":"POST"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var body httpStatusResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "invalid_trigger" {
		t.Fatalf("status body = %q, want invalid_trigger", body.Status)
	}
}

func TestHTTPAdminTriggerRespectsAcceptedReplyAsync(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	scripted := &asyncAdminTriggerProvider{started: make(chan struct{})}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /jobs"),
		Pipe("process job", Model("anthropic/claude-sonnet-4-6")),
		Reply(Accepted()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerRequest(t, "", "POST", "/jobs", `{"id":"J-1"}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var body adminTriggerResponse
	decodeAdminJSON(t, rec, &body)
	if body.Status != "accepted" || body.ExecID == "" || body.TraceID == "" || body.SessionID == "" {
		t.Fatalf("body = %+v, want accepted with execution identifiers", body)
	}
	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("async admin trigger did not start provider")
	}
}

func TestHTTPAdminTriggerRunsCronPlanThroughHarness(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"cron"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Cron("0 6 * * *")),
		Pipe("summarize cron work", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	handler, err := newAdminHandlerWithRuntime(plans, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "", `{
		"trigger": "cron",
		"expr": "0 6 * * *",
		"scheduled_at": "2026-05-21T06:00:00Z"
	}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "trigger", `"cron"`)
	assertRawJSONField(t, input, "expr", `"0 6 * * *"`)
	assertRawJSONField(t, input, "scheduled_at", `"2026-05-21T06:00:00Z"`)
}

func TestHTTPAdminTriggerRedactsCronPushOutput(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	webhook, posts := newWebhookPostRecorder(t)
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"cron","api_key":"sk-cron"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Cron("0 6 * * *")),
		Pipe("summarize cron work", Model("anthropic/claude-haiku-4-5")),
		Push(Webhook(webhook.URL)),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	handler, err := newAdminHandlerWithRuntime(plans, httpRuntime{
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("webhook"),
	})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "", `{
		"trigger": "cron",
		"expr": "0 6 * * *"
	}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-cron") {
		t.Fatalf("admin trigger response leaked secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Fatalf("admin trigger response = %s, want redacted marker", rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"status":"cron","api_key":"sk-cron"}`)
}

func TestHTTPAdminTriggerRunsStreamPlanThroughHarness(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Pipe("summarize stream work", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	handler, err := newAdminHandlerWithRuntime(plans, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "", `{
		"trigger": "stream",
		"uri": "kafka://tickets",
		"id": "msg-1",
		"body": {"event": "created"},
		"metadata": {"partition": "7"}
	}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "trigger", `"stream"`)
	assertRawJSONField(t, input, "uri", `"kafka://tickets"`)
	assertRawJSONField(t, input, "id", `"msg-1"`)
	assertRawJSONField(t, input, "body", `{"event":"created"}`)
	assertRawJSONField(t, input, "metadata", `{"partition":"7"}`)
}

func TestHTTPAdminTriggerRedactsStreamPushOutput(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	webhook, posts := newWebhookPostRecorder(t)
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"stream","accessToken":"stream-token"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Pipe("summarize stream work", Model("anthropic/claude-haiku-4-5")),
		Push(Webhook(webhook.URL)),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	handler, err := newAdminHandlerWithRuntime(plans, httpRuntime{
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("webhook"),
	})
	if err != nil {
		t.Fatalf("newAdminHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, newAdminTriggerJSONRequest(t, "", `{
		"trigger": "stream",
		"uri": "kafka://tickets",
		"id": "msg-1",
		"body": {"event": "created"}
	}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "stream-token") {
		t.Fatalf("admin trigger response leaked secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Fatalf("admin trigger response = %s, want redacted marker", rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"status":"stream","accessToken":"stream-token"}`)
}

func newTestAdminTriggerHTTPHandler(t *testing.T, rt httpRuntime) http.Handler {
	t.Helper()
	if strings.TrimSpace(rt.adminToken) == "" {
		t.Setenv("OUVRIER_ENV", "dev")
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	return handler
}

func newTestParameterizedAdminTriggerHTTPHandler(t *testing.T, rt httpRuntime) http.Handler {
	t.Helper()
	if strings.TrimSpace(rt.adminToken) == "" {
		t.Setenv("OUVRIER_ENV", "dev")
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets/{id}"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	return handler
}

func newAdminTriggerRequest(t *testing.T, token, method, path, body string) *http.Request {
	t.Helper()
	payload, err := json.Marshal(struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}{
		Method: method,
		Path:   path,
		Body:   body,
	})
	if err != nil {
		t.Fatalf("Marshal admin trigger payload returned error: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/trigger", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func newAdminTriggerJSONRequest(t *testing.T, token, payload string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/trigger", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

type asyncAdminTriggerProvider struct {
	started chan struct{}
}

func (p *asyncAdminTriggerProvider) Name() string {
	return "async-admin-trigger-provider"
}

func (p *asyncAdminTriggerProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	close(p.started)
	return provider.Response{Text: `{"status":"accepted"}`, StopReason: provider.StopEndTurn}, nil
}
