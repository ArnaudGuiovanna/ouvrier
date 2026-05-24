package provider_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestTransientErrorClassifiesProviderErrors(t *testing.T) {
	boom := errors.New("rate limited")
	err := provider.TransientError(boom)

	if !errors.Is(err, boom) {
		t.Fatalf("TransientError does not unwrap original error")
	}
	if !provider.IsTransientError(err) {
		t.Fatalf("IsTransientError = false, want true")
	}
	if provider.IsTransientError(errors.New("bad request")) {
		t.Fatalf("IsTransientError = true for unclassified error")
	}
}

func TestAnthropicClassifiesHTTPStatusErrors(t *testing.T) {
	tests := []struct {
		name      string
		code      int
		kind      provider.ErrorKind
		transient bool
	}{
		{name: "rate limit", code: http.StatusTooManyRequests, kind: provider.ErrorRateLimit, transient: true},
		{name: "server error", code: http.StatusBadGateway, kind: provider.ErrorTransient, transient: true},
		{name: "auth", code: http.StatusUnauthorized, kind: provider.ErrorAuth, transient: false},
		{name: "validation", code: http.StatusBadRequest, kind: provider.ErrorValidation, transient: false},
		{name: "not found", code: http.StatusNotFound, kind: provider.ErrorPermanent, transient: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				http.Error(w, "provider failed", tt.code)
			}))
			defer server.Close()

			p, err := provider.NewAnthropic(provider.AnthropicConfig{
				APIKey:  "test-key",
				BaseURL: server.URL,
			})
			if err != nil {
				t.Fatalf("NewAnthropic returned error: %v", err)
			}

			_, err = p.Complete(context.Background(), provider.Request{
				Model:    "anthropic/claude-sonnet-4-6",
				Messages: []provider.Message{provider.UserText("hello")},
			})
			if err == nil {
				t.Fatal("Complete returned nil error")
			}
			if provider.IsTransientError(err) != tt.transient {
				t.Fatalf("IsTransientError(%v) = %v, want %v", err, provider.IsTransientError(err), tt.transient)
			}
			var classified provider.ClassifiedError
			if !errors.As(err, &classified) {
				t.Fatalf("Complete error = %v, want ClassifiedError", err)
			}
			if classified.Kind != tt.kind {
				t.Fatalf("error kind = %q, want %q", classified.Kind, tt.kind)
			}
		})
	}
}
