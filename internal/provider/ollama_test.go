package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ouvrier/internal/provider"
)

func TestNewOllamaUsesDefaultBaseURL(t *testing.T) {
	p := provider.NewOllama(provider.OllamaConfig{})
	if p.Name() != "ollama" {
		t.Fatalf("Name = %q, want ollama", p.Name())
	}
	if p.BaseURL() != "http://localhost:11434" {
		t.Fatalf("BaseURL = %q, want default Ollama endpoint", p.BaseURL())
	}
}

func TestOllamaCompleteSendsChatRequestAndParsesText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/chat" {
			t.Fatalf("path = %q", r.URL.Path)
		}

		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		assertNestedString(t, got, "llama3.1", "model")
		if nestedValue(t, got, "stream") != false {
			t.Fatalf("stream = %v, want false", nestedValue(t, got, "stream"))
		}
		assertNestedString(t, got, "system", "messages", 0, "role")
		assertNestedString(t, got, "You are concise.", "messages", 0, "content")
		assertNestedString(t, got, "user", "messages", 1, "role")
		assertNestedString(t, got, "hello", "messages", 1, "content")
		assertNestedString(t, got, "function", "tools", 0, "type")
		assertNestedString(t, got, "lookup_ticket", "tools", 0, "function", "name")
		assertNestedString(t, got, "object", "tools", 0, "function", "parameters", "type")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "llama3.1",
			"message": {"role": "assistant", "content": "done"},
			"done": true,
			"done_reason": "stop",
			"prompt_eval_count": 9,
			"eval_count": 4
		}`))
	}))
	defer server.Close()

	p := provider.NewOllama(provider.OllamaConfig{BaseURL: server.URL})
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "ollama/llama3.1",
		System:   "You are concise.",
		Messages: []provider.Message{provider.UserText("hello")},
		Tools: []provider.ToolSpec{{
			Name:        "lookup_ticket",
			Description: "Lookup ticket",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text != "done" {
		t.Fatalf("Text = %q, want done", resp.Text)
	}
	if resp.StopReason != provider.StopEndTurn {
		t.Fatalf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("Usage = %+v, want input 9 output 4", resp.Usage)
	}
}

func TestOllamaCompleteParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "qwen3",
			"message": {
				"role": "assistant",
				"content": "need lookup",
				"tool_calls": [{
					"function": {
						"name": "lookup_ticket",
						"arguments": {"id": "T-1"}
					}
				}]
			},
			"done": true,
			"done_reason": "stop"
		}`))
	}))
	defer server.Close()

	p := provider.NewOllama(provider.OllamaConfig{BaseURL: server.URL})
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "ollama/qwen3",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text != "need lookup" {
		t.Fatalf("Text = %q, want need lookup", resp.Text)
	}
	if resp.StopReason != provider.StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "ollama_tool_0" || call.Name != "lookup_ticket" {
		t.Fatalf("ToolCall = %+v, want generated ID and lookup_ticket", call)
	}
	assertProviderJSONEqual(t, call.Arguments, `{"id":"T-1"}`)
}

func TestOllamaCompleteSendsToolResultMessages(t *testing.T) {
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup_ticket",
		Arguments: json.RawMessage(`{"id":"T-1"}`),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		assertNestedString(t, got, "assistant", "messages", 1, "role")
		assertNestedString(t, got, "lookup_ticket", "messages", 1, "tool_calls", 0, "function", "name")
		assertNestedString(t, got, "tool", "messages", 2, "role")
		assertNestedString(t, got, "lookup_ticket", "messages", 2, "tool_name")
		assertNestedString(t, got, `{"status":"open"}`, "messages", 2, "content")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "qwen3",
			"message": {"role": "assistant", "content": "ticket is open"},
			"done": true,
			"done_reason": "stop"
		}`))
	}))
	defer server.Close()

	p := provider.NewOllama(provider.OllamaConfig{BaseURL: server.URL})
	_, err := p.Complete(context.Background(), provider.Request{
		Model: "ollama/qwen3",
		Messages: []provider.Message{
			provider.UserText("hello"),
			provider.AssistantToolCalls("", call),
			provider.ToolResultMessage(provider.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    json.RawMessage(`{"status":"open"}`),
			}),
		},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
}

func TestOllamaCompleteRejectsWrongProviderWithoutCallingServer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer server.Close()

	p := provider.NewOllama(provider.OllamaConfig{BaseURL: server.URL})
	_, err := p.Complete(context.Background(), provider.Request{
		Model:    "gemini/gemini-2.0-flash",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err == nil {
		t.Fatal("Complete returned nil error for wrong provider")
	}
	if calls != 0 {
		t.Fatalf("server calls = %d, want 0", calls)
	}
}
