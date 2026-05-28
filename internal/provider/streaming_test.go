package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func decodeJSON(req *http.Request, dst any) error {
	return json.NewDecoder(req.Body).Decode(dst)
}

func TestAnthropicImplementsStreamingProvider(t *testing.T) {
	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}
	if _, ok := provider.Provider(p).(provider.StreamingProvider); !ok {
		t.Fatal("Anthropic does not implement StreamingProvider")
	}
}

func TestAnthropicCompleteStreamEmitsDeltas(t *testing.T) {
	const sse = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":11}}

event: message_stop
data: {"type":"message_stop"}

`
	var gotStream bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = decodeJSON(req, &body)
		if body["stream"] == true {
			gotStream = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "k", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{provider.UserText("hi")},
	}, func(d provider.Delta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if !gotStream {
		t.Fatal("request did not set stream=true")
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %v, want Hello", deltas)
	}
	if resp.Text != "Hello" {
		t.Fatalf("resp.Text = %q, want Hello", resp.Text)
	}
	if resp.StopReason != provider.StopEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.OutputTokens != 11 {
		t.Fatalf("output tokens = %d, want 11", resp.Usage.OutputTokens)
	}
}

func TestAnthropicCompleteStreamNilCallback(t *testing.T) {
	const sse = `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: message_stop
data: {"type":"message_stop"}

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()
	p, _ := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "k", BaseURL: server.URL})
	resp, err := p.CompleteStream(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{provider.UserText("hi")},
	}, nil)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("resp.Text = %q, want ok", resp.Text)
	}
}

func TestOpenAICompatImplementsStreamingProvider(t *testing.T) {
	p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if _, ok := provider.Provider(p).(provider.StreamingProvider); !ok {
		t.Fatal("OpenAI does not implement StreamingProvider")
	}
}

func TestOpenAICompatCompleteStreamEmitsDeltas(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":11}}

data: [DONE]

`
	var gotStream bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = decodeJSON(req, &body)
		if body["stream"] == true {
			gotStream = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer server.Close()

	p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "k", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	var deltas []string
	resp, err := p.CompleteStream(context.Background(), provider.Request{
		Model:    "openai/gpt-4o-mini",
		Messages: []provider.Message{provider.UserText("hi")},
	}, func(d provider.Delta) {
		deltas = append(deltas, d.Text)
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if !gotStream {
		t.Fatal("request did not set stream=true")
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %v, want Hello", deltas)
	}
	if resp.Text != "Hello" {
		t.Fatalf("resp.Text = %q, want Hello", resp.Text)
	}
	if resp.StopReason != provider.StopEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.OutputTokens != 11 {
		t.Fatalf("output tokens = %d, want 11", resp.Usage.OutputTokens)
	}
}
