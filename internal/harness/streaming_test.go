package harness_test

import (
	"context"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// streamingScriptedProvider implements both Provider and StreamingProvider so
// tests can assert the harness prefers CompleteStream when streaming is enabled.
type streamingScriptedProvider struct {
	scriptedProvider
	deltas        []string
	streamCalls   int
	completeCalls int
}

func (p *streamingScriptedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.completeCalls++
	return p.scriptedProvider.Complete(ctx, req)
}

func (p *streamingScriptedProvider) CompleteStream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	p.streamCalls++
	for _, d := range p.deltas {
		if onDelta != nil {
			onDelta(provider.Delta{Text: d})
		}
	}
	// Record the request and return the scripted response without re-incrementing completeCalls.
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return provider.Response{}, nil
	}
	return p.responses[0], nil
}

func TestRunStreamsDeltasToEventStream(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	p := &streamingScriptedProvider{
		deltas: []string{"Hel", "lo", " world"},
	}
	p.responses = []provider.Response{{
		Text:       "Hello world",
		StopReason: provider.StopEndTurn,
		Usage:      provider.Usage{InputTokens: 3, OutputTokens: 5},
	}}

	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithStreaming(true),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Text != "Hello world" {
		t.Fatalf("Text = %q, want Hello world", out.Text)
	}
	if p.streamCalls != 1 {
		t.Fatalf("streamCalls = %d, want 1", p.streamCalls)
	}
	if p.completeCalls != 0 {
		t.Fatalf("completeCalls = %d, want 0 (should use streaming path)", p.completeCalls)
	}

	var got []string
	for _, ev := range stream.List() {
		if ev.Kind != events.EventLLMTokenDelta {
			continue
		}
		text, _ := ev.Payload["text"].(string)
		got = append(got, text)
	}
	if len(got) != 3 {
		t.Fatalf("delta events = %v, want 3", got)
	}
	if got[0] != "Hel" || got[1] != "lo" || got[2] != " world" {
		t.Fatalf("delta texts = %v, want [Hel lo  world]", got)
	}
}

func TestRunStreamingFallsBackToComplete(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	// scriptedProvider implements only Complete (non-streaming).
	p := &scriptedProvider{
		responses: []provider.Response{{
			Text:       "done",
			StopReason: provider.StopEndTurn,
		}},
	}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
		harness.WithStreaming(true),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out, err := h.Run(context.Background(), "payload")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Text != "done" {
		t.Fatalf("Text = %q, want done", out.Text)
	}
	for _, ev := range stream.List() {
		if ev.Kind == events.EventLLMTokenDelta {
			t.Fatalf("non-streaming provider emitted a token delta event: %+v", ev)
		}
	}
}

func TestRunStreamingDisabledByDefault(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	p := &streamingScriptedProvider{deltas: []string{"a", "b"}}
	p.responses = []provider.Response{{Text: "ab", StopReason: provider.StopEndTurn}}
	h, err := harness.New(p,
		harness.WithModel("anthropic/claude-sonnet-4-6"),
		harness.WithEventStream(stream),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := h.Run(context.Background(), "payload"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if p.streamCalls != 0 {
		t.Fatalf("streamCalls = %d, want 0 when streaming disabled", p.streamCalls)
	}
	if p.completeCalls != 1 {
		t.Fatalf("completeCalls = %d, want 1", p.completeCalls)
	}
}
