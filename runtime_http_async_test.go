package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ouvrier/internal/provider"
)

type blockingProvider struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

type cancelObservedProvider struct {
	started  chan struct{}
	canceled chan struct{}
}

func newCancelObservedProvider() *cancelObservedProvider {
	return &cancelObservedProvider{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (p *cancelObservedProvider) Name() string {
	return "anthropic"
}

func (p *cancelObservedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	close(p.started)
	<-ctx.Done()
	close(p.canceled)
	return provider.Response{}, ctx.Err()
}

func (p *blockingProvider) Name() string {
	return "anthropic"
}

func (p *blockingProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	close(p.started)
	select {
	case <-p.release:
		return provider.Response{Text: "done", StopReason: provider.StopEndTurn}, nil
	case <-ctx.Done():
		return provider.Response{}, ctx.Err()
	}
}

func TestNewHTTPHandlerRepliesAcceptedBeforePipelineCompletes(t *testing.T) {
	scripted := newBlockingProvider()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /jobs"),
		Pipe("process job", Model("anthropic/claude-sonnet-4-6")),
		Reply(Accepted()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(`{"id":"J-1"}`))
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		handler.ServeHTTP(rec, req)
	}()

	select {
	case <-returned:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("handler did not return before async pipeline completed")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}

	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("async pipeline did not start")
	}
	close(scripted.release)
}

func TestNewHTTPHandlerRejectsAcceptedRequestWhenWorkerPoolFull(t *testing.T) {
	scripted := newBlockingProvider()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /jobs", WorkerPool(1)),
		Pipe("process job", Model("anthropic/claude-sonnet-4-6")),
		Reply(Accepted()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(`{"id":"J-1"}`)))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", first.Code, http.StatusAccepted)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(`{"id":"J-2"}`)))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", second.Code, http.StatusTooManyRequests)
	}

	close(scripted.release)
}

func TestNewHTTPHandlerCancelsAcceptedWorkOnShutdown(t *testing.T) {
	scripted := newCancelObservedProvider()
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /jobs"),
		Pipe("process job", Model("anthropic/claude-sonnet-4-6")),
		Reply(Accepted()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(`{"id":"J-1"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("async pipeline did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.(interface{ Shutdown(context.Context) error }).Shutdown(shutdownCtx); err != nil {
		t.Fatalf("handler shutdown returned error: %v", err)
	}
	select {
	case <-scripted.canceled:
	case <-time.After(time.Second):
		t.Fatal("async pipeline was not canceled during shutdown")
	}
}

func TestHTTPAdminTriggerCancelsAcceptedWorkOnShutdown(t *testing.T) {
	t.Setenv("PIP_ENV", "dev")
	scripted := newCancelObservedProvider()
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
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	select {
	case <-scripted.started:
	case <-time.After(time.Second):
		t.Fatal("async admin trigger did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.(interface{ Shutdown(context.Context) error }).Shutdown(shutdownCtx); err != nil {
		t.Fatalf("handler shutdown returned error: %v", err)
	}
	select {
	case <-scripted.canceled:
	case <-time.After(time.Second):
		t.Fatal("async admin trigger was not canceled during shutdown")
	}
}
