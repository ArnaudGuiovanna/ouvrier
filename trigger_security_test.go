package ovr

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/events"
	"ouvrier/internal/state"
)

func TestCompilePlansCompilesIdempotencyKeyOption(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /hooks", IdempotencyKey("X-Delivery-ID")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if got := plans[0].Trigger.IdempotencyHeader; got != "X-Delivery-ID" {
		t.Fatalf("idempotency header = %q, want X-Delivery-ID", got)
	}
}

func TestValidateRejectsInvalidIdempotencyKeyOption(t *testing.T) {
	err := Validate(
		From("POST /hooks", IdempotencyKey(" ")),
		Sink(Log()),
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
	}
}

func TestCompilePlansCompilesVerifySignatureOption(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /hooks", VerifySignature("OVR_WEBHOOK_SECRET", "X-Hub-Signature-256")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	trigger := plans[0].Trigger
	if trigger.SignatureEnv != "OVR_WEBHOOK_SECRET" {
		t.Fatalf("signature env = %q, want OVR_WEBHOOK_SECRET", trigger.SignatureEnv)
	}
	if trigger.SignatureHeader != "X-Hub-Signature-256" {
		t.Fatalf("signature header = %q, want X-Hub-Signature-256", trigger.SignatureHeader)
	}
}

func TestValidateRejectsInvalidVerifySignatureOptions(t *testing.T) {
	tests := []struct {
		name string
		opt  FromOption
	}{
		{name: "empty env", opt: VerifySignature(" ", "X-Signature")},
		{name: "empty header", opt: VerifySignature("OVR_WEBHOOK_SECRET", " ")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From("POST /hooks", tt.opt),
				Sink(Log()),
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}

func TestNewHTTPHandlerRejectsSignedTriggerWhenSecretIsMissing(t *testing.T) {
	t.Setenv("OVR_MISSING_WEBHOOK_SECRET", "")
	_, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", VerifySignature("OVR_MISSING_WEBHOOK_SECRET", "X-Hub-Signature-256")),
		Sink(Log()),
	}, httpRuntime{})
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("newHTTPHandlerWithRuntime error = %v, want ErrInvalidNode", err)
	}
}

func TestNewHTTPHandlerRejectsIdempotentTriggerWithoutStateStore(t *testing.T) {
	_, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", IdempotencyKey("X-Delivery-ID")),
		Sink(Log()),
	}, httpRuntime{})
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("newHTTPHandlerWithRuntime error = %v, want ErrInvalidNode", err)
	}
}

func TestNewHTTPHandlerRejectsIdempotentTriggerWithoutHeader(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", IdempotencyKey("X-Delivery-ID")),
		Sink(Log()),
	}, httpRuntime{stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"event":"push"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertNoSinkLoggedEvent(t, stream)
}

func TestNewHTTPHandlerDoesNotDuplicateIdempotentTriggerSideEffects(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", IdempotencyKey("X-Delivery-ID")),
		Sink(Log()),
	}, httpRuntime{stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"event":"first"}`))
	firstReq.Header.Set("X-Delivery-ID", "delivery-1")
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d, body=%s", first.Code, http.StatusAccepted, first.Body.String())
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"event":"duplicate"}`))
	secondReq.Header.Set("X-Delivery-ID", "delivery-1")
	handler.ServeHTTP(second, secondReq)
	if second.Code != http.StatusAccepted {
		t.Fatalf("second status = %d, want %d, body=%s", second.Code, http.StatusAccepted, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "duplicate_idempotency_key") {
		t.Fatalf("second body = %s, want duplicate_idempotency_key", second.Body.String())
	}

	sinkEvents := 0
	idempotencyEvents := 0
	duplicateDecisions := 0
	for _, event := range stream.List() {
		switch event.Kind {
		case events.EventSinkLogged:
			sinkEvents++
			if event.Payload["input"] != `{"event":"first"}` {
				t.Fatalf("sink event payload = %+v, want first input only", event.Payload)
			}
		case events.EventIdempotencyDecision:
			idempotencyEvents++
			if event.Payload["decision"] == "duplicate" {
				duplicateDecisions++
			}
			if _, leaked := event.Payload["key"]; leaked {
				t.Fatalf("idempotency event leaked key payload: %+v", event.Payload)
			}
		}
	}
	if sinkEvents != 1 {
		t.Fatalf("sink events = %d, want one side effect", sinkEvents)
	}
	if idempotencyEvents != 2 || duplicateDecisions != 1 {
		t.Fatalf("idempotency events=%d duplicate=%d, want reserved and duplicate decisions", idempotencyEvents, duplicateDecisions)
	}
}

func TestNewHTTPHandlerReturnsFailureWhenTriggerIdempotencyStoreFails(t *testing.T) {
	store := failingTriggerIdempotencyStore{MemoryStore: state.NewMemoryStore()}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", IdempotencyKey("X-Delivery-ID")),
		Sink(Log()),
	}, httpRuntime{stateStore: store, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"event":"push"}`))
	req.Header.Set("X-Delivery-ID", "delivery-1")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	assertNoSinkLoggedEvent(t, stream)
}

func TestNewHTTPHandlerRejectsSignedTriggerWithoutSignature(t *testing.T) {
	t.Setenv("OVR_TEST_WEBHOOK_SECRET", "secret")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", VerifySignature("OVR_TEST_WEBHOOK_SECRET", "X-Hub-Signature-256")),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"event":"push"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	assertNoSinkLoggedEvent(t, stream)
	assertSignatureDecisionEvent(t, stream, "missing")
}

func TestNewHTTPHandlerRejectsSignedTriggerWithInvalidSignature(t *testing.T) {
	t.Setenv("OVR_TEST_WEBHOOK_SECRET", "secret")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", VerifySignature("OVR_TEST_WEBHOOK_SECRET", "X-Hub-Signature-256")),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(`{"event":"push"}`))
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
	assertNoSinkLoggedEvent(t, stream)
	assertSignatureDecisionEvent(t, stream, "invalid")
}

func TestNewHTTPHandlerAcceptsSignedTriggerWithValidSignature(t *testing.T) {
	const secret = "secret"
	const body = `{"event":"push"}`
	t.Setenv("OVR_TEST_WEBHOOK_SECRET", secret)
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /hooks", VerifySignature("OVR_TEST_WEBHOOK_SECRET", "X-Hub-Signature-256")),
		Sink(Log()),
	}, httpRuntime{eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hooks", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", hmacSHA256Header(secret, body))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	assertSignatureDecisionEvent(t, stream, "valid")
	assertSinkLoggedEvent(t, stream, "input", body)
}

func hmacSHA256Header(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func assertSignatureDecisionEvent(t *testing.T, stream *events.EventStream, decision string) {
	t.Helper()
	for _, event := range stream.List() {
		if event.Kind != events.EventSignatureDecision {
			continue
		}
		if event.Payload["decision"] != decision {
			continue
		}
		if _, leaked := event.Payload["signature"]; leaked {
			t.Fatalf("signature event leaked signature payload: %+v", event.Payload)
		}
		if _, leaked := event.Payload["secret"]; leaked {
			t.Fatalf("signature event leaked secret payload: %+v", event.Payload)
		}
		return
	}
	t.Fatalf("events = %+v, want signature decision %q", stream.List(), decision)
}

type failingTriggerIdempotencyStore struct {
	*state.MemoryStore
}

func (s failingTriggerIdempotencyStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
	return "", false, errors.New("reserve idempotency failed")
}
