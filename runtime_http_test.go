package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
	"ouvrier/internal/state"
)

type httpTestReply struct {
	Status string `json:"status"`
}

type httpDirectMismatchReply struct {
	Message string `json:"message"`
}

func TestNewHTTPHandlerServesDirectGETReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status body = %q, want ok", body.Status)
	}
}

func TestNewHTTPHandlerValidatesDirectReplySchema(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpDirectMismatchReply]()),
	}, httpRuntime{stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 || violations[0].SchemaName != "ovr.httpDirectMismatchReply" {
		t.Fatalf("violations = %+v, want direct reply schema violation", violations)
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaValidationFailed); !ok {
		t.Fatalf("events = %+v, want schema validation failed event", stream.List())
	}
}

func TestNewHTTPHandlerServesDirectPOSTReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("POST /tickets"),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewHTTPHandlerServesDirectAcceptedReply(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("POST /jobs"),
		Reply(Accepted()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jobs", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "accepted" {
		t.Fatalf("status body = %q, want accepted", body.Status)
	}
}

func TestNewHTTPHandlerServesHTTPPathParams(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /tickets/{id}"),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tickets/123", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestNewHTTPHandlerServesDirectSinkAsAccepted(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("POST /events"),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/events", nil))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestNewHTTPHandlerLogsDirectSinkInputToEventStream(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event":"created"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	assertSinkLoggedEvent(t, stream, "input", `{"event":"created"}`)
}

func TestNewHTTPHandlerPassesLogSinkEventThroughHookBus(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	hooks := events.NewHookBus()
	if err := hooks.Register(events.EventSinkLogged, func(ctx context.Context, event events.Event) (events.Event, error) {
		event.Payload["checked"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Sink(Log()),
	}, httpRuntime{eventStream: stream, hookBus: hooks})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event":"created"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	event := assertSinkLoggedEvent(t, stream, "input", `{"event":"created"}`)
	if event.Payload["checked"] != true {
		t.Fatalf("sink_logged payload = %+v, want hook enrichment", event.Payload)
	}
}

func TestNewHTTPHandlerWritesPipelineOutputToFileSink(t *testing.T) {
	outputRoot := t.TempDir()
	outputPath := filepath.Join(outputRoot, "tickets.jsonl")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File(outputPath)),
	}, httpRuntime{provider: scripted, toolExecutor: outputAllowedExecutor("file"), sandbox: fileSinkSandbox(t, outputRoot)})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) returned error: %v", outputPath, err)
	}
	if got := strings.TrimSpace(string(data)); got != `{"status":"classified"}` {
		t.Fatalf("file output = %q, want provider output", got)
	}
}

func TestNewHTTPHandlerLogsPipelineOutputSinkToEventStream(t *testing.T) {
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
		Sink(Log()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	event := assertSinkLoggedEvent(t, stream, "output", `{"status":"classified"}`)
	if event.ExecID == "" || event.SessionID == "" || event.TraceID == "" {
		t.Fatalf("sink_logged event = %+v, want session identifiers", event)
	}
}

func TestNewHTTPHandlerRedactsSensitiveLogSinkJSONOutput(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"password":"secret","safe":"ok"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(Log()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	assertSinkLoggedEvent(t, stream, "output", `{"password":"[REDACTED]","safe":"ok"}`)
}

func TestNewHTTPHandlerRedactsSensitiveLogSinkStringValues(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"message":"upstream failed with Authorization: Bearer root-token","safe":"ok"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(Log()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	event := assertSinkLoggedEvent(t, stream, "output", `{"message":"upstream failed with Authorization: [REDACTED]","safe":"ok"}`)
	if strings.Contains(fmt.Sprint(event.Payload), "root-token") {
		t.Fatalf("sink_logged event leaked token: %+v", event.Payload)
	}
}

func TestNewHTTPHandlerFileSinkDoesNotLogToEventStream(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	outputRoot := t.TempDir()
	outputPath := filepath.Join(outputRoot, "tickets.jsonl")
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File(outputPath)),
	}, httpRuntime{provider: scripted, eventStream: stream, toolExecutor: outputAllowedExecutor("file"), sandbox: fileSinkSandbox(t, outputRoot)})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	assertNoSinkLoggedEvent(t, stream)
}

func TestNewHTTPHandlerPushesPipelineOutputToWebhook(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{provider: scripted, toolExecutor: outputAllowedExecutor("webhook")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	assertWebhookPost(t, posts, `{"status":"classified"}`)
}

func TestNewHTTPHandlerPushesDirectInputToWebhook(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{toolExecutor: outputAllowedExecutor("webhook")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event":"created"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	assertWebhookPost(t, posts, `{"event":"created"}`)
}

func TestNewHTTPHandlerReturnsFailureWhenWebhookPushFails(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(webhook.Close)
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{provider: scripted, toolExecutor: outputAllowedExecutor("webhook")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "pipeline_execution_failed" {
		t.Fatalf("status body = %q, want pipeline_execution_failed", body.Status)
	}
}

func TestNewHTTPHandlerServesMultipleHTTPPipelines(t *testing.T) {
	handler, err := newHTTPHandler([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
		From("POST /events"),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
	}

	events := httptest.NewRecorder()
	handler.ServeHTTP(events, httptest.NewRequest(http.MethodPost, "/events", nil))
	if events.Code != http.StatusAccepted {
		t.Fatalf("events status = %d, want %d", events.Code, http.StatusAccepted)
	}
}

func TestNewHTTPHandlerReturnsProviderStatusWhenPipeProviderMissing(t *testing.T) {
	clearProviderEnv(t)

	handler, err := newHTTPHandler([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("newHTTPHandler returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "provider_not_configured" {
		t.Fatalf("status body = %q, want provider_not_configured", body.Status)
	}
}

func TestNewHTTPHandlerRejectsNonHTTPPipeline(t *testing.T) {
	_, err := newHTTPHandler([]Node{
		From(Cron("0 6 * * *")),
		Sink(Log()),
	})
	if !errors.Is(err, ErrRunNotImplemented) {
		t.Fatalf("newHTTPHandler error = %v, want ErrRunNotImplemented", err)
	}
}

func assertSinkLoggedEvent(t *testing.T, stream *events.EventStream, payloadKey, wantPayload string) events.Event {
	t.Helper()

	var matches []events.Event
	for _, event := range stream.List() {
		if event.Kind == events.EventSinkLogged {
			matches = append(matches, event)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("sink_logged events = %d, want 1; events=%+v", len(matches), stream.List())
	}
	got, ok := matches[0].Payload[payloadKey].(string)
	if !ok {
		t.Fatalf("sink_logged payload[%q] = %#v, want string", payloadKey, matches[0].Payload[payloadKey])
	}
	if got != wantPayload {
		t.Fatalf("sink_logged payload[%q] = %q, want %q", payloadKey, got, wantPayload)
	}
	return matches[0]
}

func assertNoSinkLoggedEvent(t *testing.T, stream *events.EventStream) {
	t.Helper()

	for _, event := range stream.List() {
		if event.Kind == events.EventSinkLogged {
			t.Fatalf("unexpected sink_logged event: %+v", event)
		}
	}
}

func newWebhookPostRecorder(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()

	posts := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("webhook method = %s, want POST", req.Method)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("ReadAll webhook body returned error: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		posts <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server, posts
}

func assertWebhookPost(t *testing.T, posts <-chan string, want string) {
	t.Helper()

	select {
	case got := <-posts:
		if strings.TrimSpace(got) != want {
			t.Fatalf("webhook body = %q, want %s", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("webhook did not receive POST")
	}
}
