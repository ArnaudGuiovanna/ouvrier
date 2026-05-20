package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
	"ouvrier/internal/state"
)

type httpScriptedProvider struct {
	name      string
	requests  []provider.Request
	responses []provider.Response
	response  provider.Response
	err       error
}

func (p *httpScriptedProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "scripted"
}

func (p *httpScriptedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return provider.Response{}, p.err
	}
	if len(p.responses) > 0 {
		index := len(p.requests) - 1
		if index >= len(p.responses) {
			index = len(p.responses) - 1
		}
		return p.responses[index], nil
	}
	return p.response, nil
}

func TestNewHTTPHandlerExecutesPipeThroughHarnessRuntime(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" || body.Output != `{"status":"classified"}` {
		t.Fatalf("body = %+v, want ok classified JSON", body)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].Messages[0].Text() != `{"title":"broken"}` {
		t.Fatalf("provider input = %q", scripted.requests[0].Messages[0].Text())
	}
	if !strings.Contains(scripted.requests[0].System, "classify ticket") {
		t.Fatalf("provider system prompt = %q, want Pipe goal", scripted.requests[0].System)
	}
}

func TestNewHTTPHandlerPassesPathParamsAndJSONBodyToHarnessInput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets/{id}"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets/T-123", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "body", `{"title":"broken"}`)
	assertRawJSONField(t, input, "path_params", `{"id":"T-123"}`)
}

func TestNewHTTPHandlerPassesPathParamsAndTextBodyToHarnessInput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets/{id}"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets/T-456", strings.NewReader("urgent plain text"))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "body", `"urgent plain text"`)
	assertRawJSONField(t, input, "path_params", `{"id":"T-456"}`)
}

func TestNewHTTPHandlerPassesPathParamsAndEmptyBodyToHarnessInput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"ok"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets/{id}"),
		Pipe("load ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets/T-999", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertNoJSONField(t, input, "body")
	assertRawJSONField(t, input, "path_params", `{"id":"T-999"}`)
}

func decodeProviderInput(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()

	var input map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatalf("provider input is not JSON: %v; input=%s", err, raw)
	}
	return input
}

func assertRawJSONField(t *testing.T, input map[string]json.RawMessage, field, want string) {
	t.Helper()

	raw, ok := input[field]
	if !ok {
		t.Fatalf("provider input missing %q: %+v", field, input)
	}
	var gotValue any
	if err := json.Unmarshal(raw, &gotValue); err != nil {
		t.Fatalf("provider input field %q is not JSON: %v; field=%s", field, err, string(raw))
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("test want JSON for %q is invalid: %v", field, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("provider input field %q = %s, want %s", field, string(raw), want)
	}
}

func assertNoJSONField(t *testing.T, input map[string]json.RawMessage, field string) {
	t.Helper()

	if _, ok := input[field]; ok {
		t.Fatalf("provider input field %q is present, want absent: %+v", field, input)
	}
}

func TestNewHTTPHandlerPersistsHarnessStateWhenConfigured(t *testing.T) {
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	sessions, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions returned error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	execution, ok, err := store.Execution(context.Background(), sessions[0].ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionCompleted {
		t.Fatalf("execution status = %q, want completed", execution.Status)
	}
}

func TestNewHTTPHandlerEmitsPipelineEventsThroughHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventPipelineStarted, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets/{id}"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream, hookBus: hooks})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets/T-123", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	started, ok := findRuntimeHTTPEvent(stream.List(), events.EventPipelineStarted)
	if !ok {
		t.Fatalf("events = %+v, want pipeline started event", stream.List())
	}
	if started.Payload["method"] != "POST" || started.Payload["path"] != "/tickets/{id}" || started.Payload["checked"] != true {
		t.Fatalf("pipeline started payload = %+v, want route details and hook enrichment", started.Payload)
	}
	completed, ok := findRuntimeHTTPEvent(stream.List(), events.EventPipelineCompleted)
	if !ok {
		t.Fatalf("events = %+v, want pipeline completed event", stream.List())
	}
	if completed.ExecID == "" || completed.SessionID == "" || completed.TraceID == "" {
		t.Fatalf("completed event = %+v, want session identifiers", completed)
	}
	if completed.Payload["status"] != "completed" || completed.Payload["steps"] != 1 {
		t.Fatalf("pipeline completed payload = %+v, want completed status", completed.Payload)
	}
	if _, ok := completed.Payload["output"]; ok {
		t.Fatalf("pipeline completed payload = %+v, must not include raw output", completed.Payload)
	}
}

func TestNewHTTPHandlerEmitsPipelineFailedOnProviderError(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{err: context.DeadlineExceeded}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	failed, ok := findRuntimeHTTPEvent(stream.List(), events.EventPipelineFailed)
	if !ok {
		t.Fatalf("events = %+v, want pipeline failed event", stream.List())
	}
	if failed.ExecID == "" || failed.SessionID == "" || failed.TraceID == "" {
		t.Fatalf("failed event = %+v, want session identifiers", failed)
	}
	if failed.Payload["status"] != "failed" || failed.Payload["error"] == "" {
		t.Fatalf("pipeline failed payload = %+v, want failed status and error", failed.Payload)
	}
	if _, ok := failed.Payload["input"]; ok {
		t.Fatalf("pipeline failed payload = %+v, must not include raw input", failed.Payload)
	}
}

func TestNewHTTPHandlerRecordsOutputSchemaViolation(t *testing.T) {
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Output[httpTestReply](),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(violations))
	}
	if violations[0].SchemaName != "ovr.httpTestReply" {
		t.Fatalf("schema name = %q, want ovr.httpTestReply", violations[0].SchemaName)
	}
}

func TestNewHTTPHandlerRepairsPipeOutputSchemaViolationWhenConfigured(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
			{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Output[httpTestReply](),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store, eventStream: stream, schemaRepairAttempts: 1})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Output != `{"status":"classified"}` {
		t.Fatalf("output = %q, want repaired JSON", body.Output)
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial and repair", len(scripted.requests))
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want original violation", len(violations))
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaRepairStarted); !ok {
		t.Fatalf("events = %+v, want schema repair started event", stream.List())
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaRepairCompleted); !ok {
		t.Fatalf("events = %+v, want schema repair completed event", stream.List())
	}
	if event, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaValidationPassed); !ok || event.Payload["repaired"] != true {
		t.Fatalf("events = %+v, want repaired schema validation passed event", stream.List())
	}
}

func TestNewHTTPHandlerRepairsTerminalReplySchemaViolationWhenConfigured(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
			{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
		},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store, eventStream: stream, schemaRepairAttempts: 1})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 2 {
		t.Fatalf("provider calls = %d, want initial and repair", len(scripted.requests))
	}
	if !strings.Contains(scripted.requests[0].System, "JSON Schema") ||
		!strings.Contains(scripted.requests[0].System, "ovr.httpTestReply") {
		t.Fatalf("provider system prompt = %q, want terminal reply schema guidance", scripted.requests[0].System)
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want original violation", len(violations))
	}
	if event, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaValidationPassed); !ok || event.Payload["repaired"] != true {
		t.Fatalf("events = %+v, want repaired schema validation passed event", stream.List())
	}
}

func TestNewHTTPHandlerValidatesTerminalReplySchemaWithoutPipeOutput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "pipeline_execution_failed" {
		t.Fatalf("status body = %q, want pipeline_execution_failed", body.Status)
	}
}

func TestNewHTTPHandlerRecordsTerminalReplySchemaViolationWithoutPipeOutput(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(violations))
	}
	if violations[0].SessionID == "" || violations[0].ExecID == "" {
		t.Fatalf("violation = %+v, want session identifiers", violations[0])
	}
	if violations[0].SchemaName != "ovr.httpTestReply" {
		t.Fatalf("schema name = %q, want ovr.httpTestReply", violations[0].SchemaName)
	}
	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaViolation)
	if !ok {
		t.Fatalf("events = %+v, want schema violation event", stream.List())
	}
	if event.SessionID != violations[0].SessionID || event.ExecID != violations[0].ExecID {
		t.Fatalf("event = %+v, want violation identifiers %+v", event, violations[0])
	}
}

func TestNewHTTPHandlerEmitsTerminalReplySchemaValidationPassedWithoutPipeOutput(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaValidationPassed)
	if !ok {
		t.Fatalf("events = %+v, want schema validation passed event", stream.List())
	}
	if event.SessionID == "" || event.ExecID == "" || event.TraceID == "" {
		t.Fatalf("event = %+v, want session identifiers", event)
	}
	if event.Payload["schema"] != "ovr.httpTestReply" {
		t.Fatalf("event payload = %+v, want schema ovr.httpTestReply", event.Payload)
	}
	if _, ok := event.Payload["output"]; ok {
		t.Fatalf("event payload = %+v, must not include raw output", event.Payload)
	}
}

func TestNewHTTPHandlerPassesTerminalReplySchemaEventThroughHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventSchemaValidationPassed, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream, hookBus: hooks})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaValidationPassed)
	if !ok {
		t.Fatalf("events = %+v, want schema validation passed event", stream.List())
	}
	if event.Payload["checked"] != true {
		t.Fatalf("event payload = %+v, want hook enrichment", event.Payload)
	}
}

func TestNewHTTPHandlerPassesPipeSchemaEventsThroughHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventSchemaValidationPassed, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Output[httpTestReply](),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream, hookBus: hooks})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var matches []events.Event
	for _, event := range stream.List() {
		if event.Kind == events.EventSchemaValidationPassed {
			matches = append(matches, event)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("schema validation passed events = %d, want 1; events=%+v", len(matches), stream.List())
	}
	if matches[0].Payload["checked"] != true {
		t.Fatalf("event payload = %+v, want hook enrichment", matches[0].Payload)
	}
}

func TestNewHTTPHandlerPassesPipeToolsToHarnessRuntime(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup_ticket", func(ctx context.Context, args struct {
				ID string `json:"id"`
			}) (string, error) {
				return "ticket", nil
			}, Describe("Lookup ticket."), Param("id", "Ticket ID.")),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"id":"T-1"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	tools := scripted.requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("provider tools = %d, want 1", len(tools))
	}
	if tools[0].Name != "lookup_ticket" || tools[0].Description != "Lookup ticket." {
		t.Fatalf("tool spec = %+v", tools[0])
	}
	if len(tools[0].InputSchema) == 0 {
		t.Fatal("tool input schema is empty")
	}
}

func findRuntimeHTTPEvent(recorded []events.Event, kind events.EventKind) (events.Event, bool) {
	for _, event := range recorded {
		if event.Kind == kind {
			return event, true
		}
	}
	return events.Event{}, false
}

type httpSubAgentReply struct {
	Text string `json:"text"`
}

func TestNewHTTPHandlerRunsSubAgentToolThroughChildSession(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventTaskCompleted, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		responses: []provider.Response{
			{
				Text:       "need translation",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:        "call_translate",
					Name:      "translate",
					Arguments: []byte(`{"input":"hello"}`),
				}},
			},
			{
				Text:       `{"text":"bonjour"}`,
				StopReason: provider.StopEndTurn,
			},
			{
				Text:       `{"status":"ok"}`,
				StopReason: provider.StopEndTurn,
			},
		},
	}
	translator := Pipeline(
		Pipe("translate text",
			Model("anthropic/claude-haiku-4-5"),
			Output[httpSubAgentReply](),
		),
	)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("translate", translator, MaxParallel(2)),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store, eventStream: stream, hookBus: hooks})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/emails", strings.NewReader(`{"body":"hello"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 3 {
		t.Fatalf("provider calls = %d, want root, child, root", len(scripted.requests))
	}
	if len(scripted.requests[0].Tools) != 1 || scripted.requests[0].Tools[0].Name != "translate" {
		t.Fatalf("root tools = %+v, want translate subagent", scripted.requests[0].Tools)
	}
	if scripted.requests[1].Model != "anthropic/claude-haiku-4-5" {
		t.Fatalf("child model = %q, want haiku", scripted.requests[1].Model)
	}
	if scripted.requests[1].Messages[0].Text() != "hello" {
		t.Fatalf("child input = %q, want hello", scripted.requests[1].Messages[0].Text())
	}

	lastRoot := scripted.requests[2]
	if lastRoot.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("final root model = %q, want sonnet", lastRoot.Model)
	}
	toolResult := lastRoot.Messages[len(lastRoot.Messages)-1].Blocks[0].ToolResult
	if toolResult == nil || toolResult.IsError || !strings.Contains(string(toolResult.Content), "bonjour") {
		t.Fatalf("tool result = %+v, want successful child output", toolResult)
	}

	sessions, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions returned error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want root and child", len(sessions))
	}
	var rootSessionID, execID, traceID string
	for _, session := range sessions {
		if session.ParentSessionID == "" {
			rootSessionID = session.SessionID
			execID = session.ExecID
			traceID = session.TraceID
			break
		}
	}
	if rootSessionID == "" {
		t.Fatalf("sessions = %+v, want root session", sessions)
	}
	var child bool
	var childSessionID string
	for _, session := range sessions {
		if session.ParentSessionID == "" {
			continue
		}
		child = true
		childSessionID = session.SessionID
		if session.ParentSessionID != rootSessionID || session.ExecID != execID || session.TraceID != traceID {
			t.Fatalf("child session = %+v, want parent %q exec %q trace %q", session, rootSessionID, execID, traceID)
		}
	}
	if !child {
		t.Fatalf("sessions = %+v, want root and child lineage", sessions)
	}

	var taskStarted, taskCompleted bool
	for _, event := range stream.List() {
		if event.Kind == events.EventTaskStarted && event.Payload["subagent"] == "translate" {
			taskStarted = true
		}
		if event.Kind == events.EventTaskCompleted && event.Payload["subagent"] == "translate" {
			taskCompleted = true
			if event.Payload["checked"] != true {
				t.Fatalf("task completed event = %+v, want hook enrichment", event)
			}
			if event.Payload["child_session_id"] != childSessionID {
				t.Fatalf("task completed event = %+v, want child session %q", event, childSessionID)
			}
		}
	}
	if !taskStarted || !taskCompleted {
		t.Fatalf("events = %+v, want subagent task start and completion", stream.List())
	}
}
