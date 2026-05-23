package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ouvrier/internal/provider"
)

func TestNewAnthropicRequiresAPIKey(t *testing.T) {
	_, err := provider.NewAnthropic(provider.AnthropicConfig{})
	if err == nil {
		t.Fatal("NewAnthropic returned nil error without API key")
	}
}

func TestAnthropicProviderDefaults(t *testing.T) {
	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}
	if p.Name() != "anthropic" {
		t.Fatalf("Name = %q, want anthropic", p.Name())
	}
	if p.BaseURL() != "https://api.anthropic.com" {
		t.Fatalf("BaseURL = %q, want default Anthropic endpoint", p.BaseURL())
	}
}

func TestAnthropicCompleteSendsMessagesRequest(t *testing.T) {
	var gotAuth string
	var gotVersion string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", req.URL.Path)
		}
		gotAuth = req.Header.Get("x-api-key")
		gotVersion = req.Header.Get("anthropic-version")
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "msg_1",
			"type": "message",
			"role": "assistant",
			"content": [{"type": "text", "text": "done"}],
			"model": "claude-sonnet-4-6",
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 7, "output_tokens": 11}
		}`))
	}))
	defer server.Close()

	p, err := provider.NewAnthropic(provider.AnthropicConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		System:   "Be concise.",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	if gotAuth != "test-key" {
		t.Fatalf("x-api-key = %q, want test-key", gotAuth)
	}
	if gotVersion == "" {
		t.Fatal("anthropic-version header is empty")
	}
	if gotBody["model"] != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want claude-sonnet-4-6", gotBody["model"])
	}
	if gotBody["system"] != "Be concise." {
		t.Fatalf("system = %q, want prompt", gotBody["system"])
	}
	if gotBody["max_tokens"] == nil {
		t.Fatal("max_tokens is missing")
	}

	if resp.Text != "done" {
		t.Fatalf("Text = %q, want done", resp.Text)
	}
	if resp.StopReason != provider.StopEndTurn {
		t.Fatalf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 11 {
		t.Fatalf("Usage = %+v", resp.Usage)
	}
	if resp.Metadata.Provider != "anthropic" || resp.Metadata.Model != "anthropic/claude-sonnet-4-6" || resp.Metadata.Latency <= 0 {
		t.Fatalf("Metadata = %+v, want anthropic model metadata", resp.Metadata)
	}
}

func TestAnthropicCompleteMarksSystemPromptForPromptCaching(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "done"}],
			"stop_reason": "end_turn",
			"usage": {
				"input_tokens": 7,
				"output_tokens": 11,
				"cache_creation_input_tokens": 5,
				"cache_read_input_tokens": 3
			}
		}`))
	}))
	defer server.Close()

	p, err := provider.NewAnthropic(provider.AnthropicConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		System:   "Stable harness prompt.",
		Messages: []provider.Message{provider.UserText("hello")},
		CacheKey: "prompt:test-cache-key",
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	system, ok := gotBody["system"].([]any)
	if !ok || len(system) != 1 {
		t.Fatalf("system = %#v, want one cached system block", gotBody["system"])
	}
	block, ok := system[0].(map[string]any)
	if !ok {
		t.Fatalf("system block = %#v, want object", system[0])
	}
	if block["text"] != "Stable harness prompt." {
		t.Fatalf("system text = %#v, want stable prompt", block["text"])
	}
	cache, ok := block["cache_control"].(map[string]any)
	if !ok || cache["type"] != "ephemeral" {
		t.Fatalf("cache_control = %#v, want ephemeral cache control", block["cache_control"])
	}
	if resp.Metadata.PromptCache.CacheKey != "prompt:test-cache-key" ||
		!resp.Metadata.PromptCache.Requested ||
		!resp.Metadata.PromptCache.Supported ||
		!resp.Metadata.PromptCache.Applied ||
		resp.Metadata.PromptCache.WriteInputTokens != 5 ||
		resp.Metadata.PromptCache.ReadInputTokens != 3 {
		t.Fatalf("PromptCache = %+v, want applied Anthropic prompt cache metadata", resp.Metadata.PromptCache)
	}
}

func TestAnthropicCompleteMarksLastToolForPromptCachingWhenSystemEmpty(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "done"}],
			"stop_reason": "end_turn",
			"usage": {
				"input_tokens": 7,
				"output_tokens": 11,
				"cache_creation": {"ephemeral_5m_input_tokens": 13}
			}
		}`))
	}))
	defer server.Close()

	p, err := provider.NewAnthropic(provider.AnthropicConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{provider.UserText("hello")},
		CacheKey: "prompt:test-cache-key",
		Tools: []provider.ToolSpec{{
			Name:        "lookup",
			Description: "Lookup data.",
			InputSchema: []byte(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if _, ok := gotBody["system"]; ok {
		t.Fatalf("system = %#v, want omitted system", gotBody["system"])
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one tool", gotBody["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool = %#v, want object", tools[0])
	}
	cache, ok := tool["cache_control"].(map[string]any)
	if !ok || cache["type"] != "ephemeral" {
		t.Fatalf("cache_control = %#v, want ephemeral cache control", tool["cache_control"])
	}
	if resp.Metadata.PromptCache.WriteInputTokens != 13 {
		t.Fatalf("PromptCache = %+v, want nested cache creation tokens", resp.Metadata.PromptCache)
	}
}

func TestAnthropicCompleteParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{
				"type": "tool_use",
				"id": "toolu_1",
				"name": "lookup",
				"input": {"query":"ouvrier"}
			}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 3, "output_tokens": 5}
		}`))
	}))
	defer server.Close()

	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{provider.UserText("hello")},
		Tools: []provider.ToolSpec{{
			Name:        "lookup",
			Description: "Lookup data.",
			InputSchema: []byte(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.StopReason != provider.StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "toolu_1" || call.Name != "lookup" {
		t.Fatalf("ToolCall = %+v", call)
	}
	if string(call.Arguments) != `{"query":"ouvrier"}` {
		t.Fatalf("Arguments = %s", call.Arguments)
	}
}

func TestAnthropicCompleteSerializesToolResultErrors(t *testing.T) {
	call := provider.ToolCall{ID: "toolu_1", Name: "lookup", Arguments: []byte(`{"query":"ouvrier"}`)}
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatalf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "handled"}],
			"stop_reason": "end_turn"
		}`))
	}))
	defer server.Close()

	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}
	_, err = p.Complete(context.Background(), provider.Request{
		Model: "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{
			provider.AssistantToolCalls("", call),
			provider.ToolResultMessage(provider.ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    []byte(`"lookup failed"`),
				IsError:    true,
			}),
		},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	messages := gotBody["messages"].([]any)
	content := messages[1].(map[string]any)["content"].([]any)
	block := content[0].(map[string]any)
	if block["is_error"] != true {
		t.Fatalf("tool_result is_error = %v, want true", block["is_error"])
	}
}

func TestAnthropicCompleteRejectsWrongProviderModel(t *testing.T) {
	p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewAnthropic returned error: %v", err)
	}
	_, err = p.Complete(context.Background(), provider.Request{
		Model:    "openai/gpt-4.1",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err == nil {
		t.Fatal("Complete returned nil error, want provider mismatch")
	}
}
