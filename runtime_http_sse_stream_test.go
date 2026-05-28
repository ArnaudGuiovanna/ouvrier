package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// httpStreamingProvider implements provider.StreamingProvider so the SSE path
// can be exercised end-to-end.
type httpStreamingProvider struct {
	httpScriptedProvider
	deltas      []string
	streamCalls int
}

func (p *httpStreamingProvider) CompleteStream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	p.streamCalls++
	for _, d := range p.deltas {
		if onDelta != nil {
			onDelta(provider.Delta{Text: d})
		}
	}
	p.requests = append(p.requests, req)
	return p.response, nil
}

func TestNewHTTPHandlerStreamsTokenDeltasOverSSE(t *testing.T) {
	streamer := &httpStreamingProvider{
		deltas: []string{"Hel", "lo"},
	}
	streamer.response = provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
	}, httpRuntime{provider: streamer})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if streamer.streamCalls != 1 {
		t.Fatalf("streamCalls = %d, want 1 (streaming should be enabled on SSE)", streamer.streamCalls)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: llm_token_delta\n",
		`"text":"Hel"`,
		`"text":"lo"`,
		"event: output\ndata: {\"status\":\"classified\"}\n\n",
		"event: done\ndata: {\"status\":\"completed\"}\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, body)
		}
	}
}

func TestNewHTTPHandlerDoesNotStreamWithoutSSE(t *testing.T) {
	streamer := &httpStreamingProvider{
		deltas: []string{"a", "b"},
	}
	streamer.response = provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: streamer})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if streamer.streamCalls != 0 {
		t.Fatalf("streamCalls = %d, want 0 for non-SSE reply", streamer.streamCalls)
	}
}
