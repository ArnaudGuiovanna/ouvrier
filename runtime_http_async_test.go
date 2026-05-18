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
