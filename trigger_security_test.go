package ovr

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/events"
)

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
	assertSinkLoggedEvent(t, stream, "input", body)
}

func hmacSHA256Header(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
