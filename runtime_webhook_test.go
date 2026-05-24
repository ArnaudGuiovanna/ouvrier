package ovr

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestNewWebhookHandlerServesDirectSink(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newWebhookHandlerWithRuntime([]Node{
		From(Webhook("github")),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newWebhookHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{"event":"push"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	event := assertSinkLoggedEvent(t, stream, "input", `{"body":{"event":"push"},"provider":"github","trigger":"webhook"}`)
	assertWebhookLogInput(t, event, "github", "push")
}

func TestNewWebhookHandlerRunsPipelineAndPushesOutput(t *testing.T) {
	webhook, posts := newWebhookPostRecorder(t)
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"normalized"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newWebhookHandlerWithRuntime([]Node{
		From(Webhook("github")),
		Pipe("normalize webhook", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook(webhook.URL)),
	}, httpRuntime{provider: scripted, toolExecutor: outputAllowedExecutor("webhook")})
	if err != nil {
		t.Fatalf("newWebhookHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{"event":"push"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertWebhookPost(t, posts, `{"status":"normalized"}`)
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "trigger", `"webhook"`)
	assertRawJSONField(t, input, "provider", `"github"`)
}

func TestNewWebhookHandlerRejectsNonWebhookPipeline(t *testing.T) {
	_, err := newWebhookHandlerWithRuntime([]Node{
		From("POST /hooks"),
		Sink(Log()),
	}, httpRuntime{})
	if !errors.Is(err, ErrRunNotImplemented) {
		t.Fatalf("newWebhookHandlerWithRuntime error = %v, want ErrRunNotImplemented", err)
	}
}

func TestNewWebhookHandlerAppliesSignatureAndIdempotency(t *testing.T) {
	t.Setenv("OVR_TEST_WEBHOOK_SECRET", "secret")
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newWebhookHandlerWithRuntime([]Node{
		From(Webhook("github"),
			VerifySignature("OVR_TEST_WEBHOOK_SECRET", "X-Hub-Signature-256"),
			IdempotencyKey("X-GitHub-Delivery"),
		),
		Sink(Log()),
	}, httpRuntime{stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newWebhookHandlerWithRuntime returned error: %v", err)
	}

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{"event":"push"}`))
	firstReq.Header.Set("X-GitHub-Delivery", "delivery-1")
	firstReq.Header.Set("X-Hub-Signature-256", hmacSHA256Header("secret", `{"event":"push"}`))
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d, body=%s", first.Code, http.StatusAccepted, first.Body.String())
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{"event":"duplicate"}`))
	secondReq.Header.Set("X-GitHub-Delivery", "delivery-1")
	secondReq.Header.Set("X-Hub-Signature-256", hmacSHA256Header("secret", `{"event":"duplicate"}`))
	handler.ServeHTTP(second, secondReq)
	if second.Code != http.StatusAccepted {
		t.Fatalf("second status = %d, want %d, body=%s", second.Code, http.StatusAccepted, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "duplicate_idempotency_key") {
		t.Fatalf("second body = %s, want duplicate_idempotency_key", second.Body.String())
	}

	sinkEvents := 0
	for _, event := range stream.List() {
		if event.Kind == events.EventSinkLogged {
			sinkEvents++
		}
	}
	if sinkEvents != 1 {
		t.Fatalf("sink events = %d, want one side effect", sinkEvents)
	}
}

func TestNewHTTPCompatibleHandlerServesHTTPAndWebhookPipelines(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPCompatibleHandlerWithRuntime([]Node{
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
		From(Webhook("github")),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPCompatibleHandlerWithRuntime returned error: %v", err)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
	}

	webhook := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{"event":"push"}`))
	handler.ServeHTTP(webhook, req)
	if webhook.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d, want %d, body=%s", webhook.Code, http.StatusAccepted, webhook.Body.String())
	}
	assertSinkLoggedEvent(t, stream, "input", `{"body":{"event":"push"},"provider":"github","trigger":"webhook"}`)
}

func assertWebhookLogInput(t *testing.T, event events.Event, provider, eventName string) {
	t.Helper()

	raw, ok := event.Payload["input"].(string)
	if !ok {
		t.Fatalf("sink input = %#v, want string", event.Payload["input"])
	}
	var payload struct {
		Trigger  string `json:"trigger"`
		Provider string `json:"provider"`
		Body     struct {
			Event string `json:"event"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("sink input is not JSON: %v", err)
	}
	if payload.Trigger != "webhook" || payload.Provider != provider || payload.Body.Event != eventName {
		t.Fatalf("sink input = %+v, want webhook/%s/%s", payload, provider, eventName)
	}
}
