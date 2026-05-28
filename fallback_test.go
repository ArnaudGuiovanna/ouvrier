package ovr

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestCompilePlansCompilesFallbackModels(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Fallback("openai/gpt-4.1-mini", "groq/llama-3.1"),
		),
		Reply(JSON[toolReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	step := plans[0].Steps[0]
	if len(step.Fallback) != 2 || step.Fallback[0] != "openai/gpt-4.1-mini" || step.Fallback[1] != "groq/llama-3.1" {
		t.Fatalf("Fallback = %+v, want ordered fallback models", step.Fallback)
	}
}

func TestValidateRejectsInvalidFallbackOptions(t *testing.T) {
	tests := []struct {
		name string
		opt  PipeOption
	}{
		{name: "no models", opt: Fallback()},
		{name: "empty model", opt: Fallback("")},
		{name: "missing provider", opt: Fallback("gpt-4.1-mini")},
		{name: "missing name", opt: Fallback("openai/")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From("POST /tickets"),
				Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6"), tt.opt),
				Reply(JSON[toolReply]()),
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}

func TestValidateRejectsFallbackDeclaredTwice(t *testing.T) {
	err := Validate(
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Fallback("openai/gpt-4.1-mini"),
			Fallback("groq/llama-3.1"),
		),
		Reply(JSON[toolReply]()),
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
	}
}

func TestNewHTTPHandlerFallsThroughToFallbackModelOnRateLimit(t *testing.T) {
	primary := &httpScriptedProvider{
		name: "anthropic",
		err:  provider.RateLimitError(errors.New("429 too many requests")),
	}
	fallback := &httpScriptedProvider{
		name:     "openai",
		response: provider.Response{Text: `{"status":"fallback"}`, StopReason: provider.StopEndTurn},
	}
	registry, err := provider.NewRegistry(primary, fallback)
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Fallback("openai/gpt-4.1-mini"),
			Retry(0),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{providers: registry})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(primary.requests) != 1 {
		t.Fatalf("primary calls = %d, want 1", len(primary.requests))
	}
	if len(fallback.requests) != 1 {
		t.Fatalf("fallback calls = %d, want 1 (fallthrough on rate limit)", len(fallback.requests))
	}
}

func TestNewHTTPHandlerDoesNotFallThroughOnPermanentError(t *testing.T) {
	primary := &httpScriptedProvider{
		name: "anthropic",
		err:  provider.PermanentError(errors.New("model removed")),
	}
	fallback := &httpScriptedProvider{
		name:     "openai",
		response: provider.Response{Text: `{"status":"fallback"}`, StopReason: provider.StopEndTurn},
	}
	registry, err := provider.NewRegistry(primary, fallback)
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Fallback("openai/gpt-4.1-mini"),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{providers: registry})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want failure on permanent error", rec.Code)
	}
	if len(primary.requests) != 1 {
		t.Fatalf("primary calls = %d, want 1", len(primary.requests))
	}
	if len(fallback.requests) != 0 {
		t.Fatalf("fallback calls = %d, want 0 (no fallthrough on permanent)", len(fallback.requests))
	}
}

func TestWithProviderBudgetValidation(t *testing.T) {
	if err := NewRunner(WithProviderBudget("", 2)).err; err == nil {
		t.Fatal("WithProviderBudget empty provider error = nil, want error")
	}
	if err := NewRunner(WithProviderBudget("anthropic", 0)).err; err == nil {
		t.Fatal("WithProviderBudget zero budget error = nil, want error")
	}
	runner := NewRunner(WithProviderBudget("anthropic", 3))
	if runner.err != nil {
		t.Fatalf("WithProviderBudget returned error: %v", runner.err)
	}
	if runner.providerBudgets["anthropic"] != 3 {
		t.Fatalf("providerBudgets = %+v, want anthropic=3", runner.providerBudgets)
	}
}
