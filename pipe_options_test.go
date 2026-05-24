package ovr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestCompilePlansCompilesPipeBudgetAndExecutionOptions(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Timeout("2s"),
			MaxTokens(123),
			MaxCostUSD(0.75),
			NoCache(),
			SequentialTools(),
			Tool("lookup", func(context.Context) error { return nil },
				ReadOnly(),
				ToolTimeout("50ms"),
			),
		),
		Reply(JSON[toolReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	step := plans[0].Steps[0]
	if step.Budget.MaxWallClock != 2*time.Second {
		t.Fatalf("MaxWallClock = %s, want 2s", step.Budget.MaxWallClock)
	}
	if step.Budget.MaxTokens != 123 {
		t.Fatalf("MaxTokens = %d, want 123", step.Budget.MaxTokens)
	}
	if step.Budget.MaxCostUSD != 0.75 {
		t.Fatalf("MaxCostUSD = %f, want 0.75", step.Budget.MaxCostUSD)
	}
	if !step.NoCache {
		t.Fatal("NoCache = false, want true")
	}
	if !step.SequentialTools {
		t.Fatal("SequentialTools = false, want true")
	}
	if got := step.Tools[0].Timeout; got != 50*time.Millisecond {
		t.Fatalf("tool timeout = %s, want 50ms", got)
	}
}

func TestCompilePlansCompilesBashCapability(t *testing.T) {
	root := t.TempDir()
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("inspect logs",
			Model("anthropic/claude-sonnet-4-6"),
			Bash(Sandbox(root, AllowEnv("PATH")), UnsafeBashHostExecution(), BashTimeout("2s"), BashMaxOutputBytes(1024)),
		),
		Reply(JSON[toolReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	bashes := plans[0].Steps[0].Bash
	if len(bashes) != 1 {
		t.Fatalf("bash tools = %d, want 1", len(bashes))
	}
	if bashes[0].Name != "bash" || bashes[0].SandboxRoot != root {
		t.Fatalf("bash plan = %+v, want default name and sandbox root", bashes[0])
	}
	if bashes[0].Timeout != 2*time.Second || bashes[0].MaxOutputBytes != 1024 {
		t.Fatalf("bash limits = timeout %s output %d", bashes[0].Timeout, bashes[0].MaxOutputBytes)
	}
	if !bashes[0].UnsafeHostExecution {
		t.Fatal("UnsafeHostExecution = false, want true")
	}
	if len(bashes[0].AllowedEnv) != 1 || bashes[0].AllowedEnv[0] != "PATH" {
		t.Fatalf("allowed env = %+v, want PATH", bashes[0].AllowedEnv)
	}
}

func TestValidateRejectsInvalidPipeBudgetAndToolTimeoutOptions(t *testing.T) {
	tests := []struct {
		name string
		opt  PipeOption
	}{
		{name: "bad timeout", opt: Timeout("bad")},
		{name: "zero timeout", opt: Timeout("0s")},
		{name: "negative timeout", opt: Timeout("-1s")},
		{name: "zero max tokens", opt: MaxTokens(0)},
		{name: "negative max tokens", opt: MaxTokens(-1)},
		{name: "zero max cost", opt: MaxCostUSD(0)},
		{name: "negative max cost", opt: MaxCostUSD(-0.1)},
		{
			name: "bad tool timeout",
			opt:  Tool("lookup", func(context.Context) error { return nil }, ToolTimeout("bad")),
		},
		{
			name: "zero tool timeout",
			opt:  Tool("lookup", func(context.Context) error { return nil }, ToolTimeout("0s")),
		},
		{
			name: "negative tool timeout",
			opt:  Tool("lookup", func(context.Context) error { return nil }, ToolTimeout("-1s")),
		},
		{name: "empty bash sandbox", opt: Bash(Sandbox(" "))},
		{name: "bad bash timeout", opt: Bash(Sandbox(t.TempDir()), BashTimeout("bad"))},
		{name: "zero bash timeout", opt: Bash(Sandbox(t.TempDir()), BashTimeout("0s"))},
		{name: "zero bash output", opt: Bash(Sandbox(t.TempDir()), BashMaxOutputBytes(0))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From("POST /tickets"),
				Pipe("classify ticket",
					Model("anthropic/claude-sonnet-4-6"),
					tt.opt,
				),
				Reply(JSON[toolReply]()),
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}

func TestNewHTTPHandlerSetsProviderCacheKeyByDefault(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[toolReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].CacheKey == "" {
		t.Fatal("provider CacheKey is empty, want default prompt cache key")
	}
}

func TestNewHTTPHandlerNoCacheClearsProviderCacheKey(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			NoCache(),
		),
		Reply(JSON[toolReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].CacheKey != "" {
		t.Fatalf("provider CacheKey = %q, want empty with NoCache", scripted.requests[0].CacheKey)
	}
}

func TestNewHTTPHandlerAppliesPipeTimeoutBudget(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &pipeTimeoutProvider{started: make(chan struct{})}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Timeout("10ms"),
		),
		Reply(JSON[toolReply]()),
	}, httpRuntime{provider: scripted, eventStream: stream})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	select {
	case <-scripted.started:
	default:
		t.Fatal("provider was not called")
	}
	foundWallClockBudget := false
	for _, event := range stream.List() {
		if event.Kind == events.EventBudgetExceeded &&
			event.Payload["budget"] == "wallclock" &&
			event.Payload["max_wallclock_ms"] == int64(10) {
			foundWallClockBudget = true
			break
		}
	}
	if !foundWallClockBudget {
		t.Fatalf("events = %+v, want wallclock budget exceeded event", stream.List())
	}
}

type pipeTimeoutProvider struct {
	once    sync.Once
	started chan struct{}
}

func (p *pipeTimeoutProvider) Name() string {
	return "pipe-timeout-scripted"
}

func (p *pipeTimeoutProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.once.Do(func() {
		close(p.started)
	})
	<-ctx.Done()
	return provider.Response{}, ctx.Err()
}
