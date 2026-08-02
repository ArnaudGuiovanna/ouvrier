package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
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

func TestNewHTTPHandlerStatefullyRedactsSplitDeltasFromEventsStoreAndSSE(t *testing.T) {
	const secret = "split-bearer-secret-value"
	streamer := &httpStreamingProvider{deltas: []string{"progress Bea", "rer " + secret[:8], secret[8:] + " done"}}
	streamer.response = provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	store := state.NewMemoryStore()

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
	}, httpRuntime{provider: streamer, eventStream: stream, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), "[REDACTED]") {
		t.Fatalf("SSE split-secret redaction failed: %s", rec.Body.String())
	}

	var liveDeltas strings.Builder
	for _, event := range stream.List() {
		if encoded := eventPayloadText(event); strings.Contains(encoded, secret) {
			t.Fatalf("in-memory event leaked split secret: %+v", event)
		}
		if event.Kind == events.EventLLMTokenDelta {
			if text, ok := event.Payload["text"].(string); ok {
				liveDeltas.WriteString(text)
			}
		}
	}
	if strings.Contains(liveDeltas.String(), secret) {
		t.Fatalf("concatenated in-memory deltas leaked split secret: %q", liveDeltas.String())
	}
	recorded, err := store.EventsSince(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	var durableDeltas strings.Builder
	for _, event := range recorded {
		if encoded := eventPayloadText(event); strings.Contains(encoded, secret) {
			t.Fatalf("stored event leaked split secret: %+v", event)
		}
		if event.Kind == events.EventLLMTokenDelta {
			if text, ok := event.Payload["text"].(string); ok {
				durableDeltas.WriteString(text)
			}
		}
	}
	if strings.Contains(durableDeltas.String(), secret) {
		t.Fatalf("concatenated stored deltas leaked split secret: %q", durableDeltas.String())
	}
}

func eventPayloadText(event events.Event) string {
	var joined strings.Builder
	for key, value := range event.Payload {
		joined.WriteString(key)
		joined.WriteString("=")
		if text, ok := value.(string); ok {
			joined.WriteString(text)
		}
	}
	return joined.String()
}
