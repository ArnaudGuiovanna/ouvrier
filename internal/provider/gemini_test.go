package provider_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestNewGeminiRequiresAPIKey(t *testing.T) {
	_, err := provider.NewGemini(provider.GeminiConfig{})
	if err == nil {
		t.Fatal("NewGemini returned nil error without API key")
	}
}

func TestGeminiCompleteSendsGenerateContentRequestAndParsesText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1beta/models/gemini-2.0-flash:generateContent" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Fatalf("x-goog-api-key = %q", r.Header.Get("x-goog-api-key"))
		}
		if r.URL.Query().Get("key") != "" {
			t.Fatalf("key query = %q, want empty", r.URL.Query().Get("key"))
		}

		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		assertNestedString(t, got, "You are concise.", "systemInstruction", "parts", 0, "text")
		assertNestedString(t, got, "user", "contents", 0, "role")
		assertNestedString(t, got, "hello", "contents", 0, "parts", 0, "text")
		assertNestedString(t, got, "lookup_ticket", "tools", 0, "functionDeclarations", 0, "name")
		assertNestedString(t, got, "Lookup ticket", "tools", 0, "functionDeclarations", 0, "description")
		assertNestedString(t, got, "object", "tools", 0, "functionDeclarations", 0, "parameters", "type")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "done"}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 7,
				"candidatesTokenCount": 5
			}
		}`))
	}))
	defer server.Close()

	p, err := provider.NewGemini(provider.GeminiConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewGemini returned error: %v", err)
	}
	if p.Name() != "gemini" {
		t.Fatalf("Name = %q, want gemini", p.Name())
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "gemini/gemini-2.0-flash",
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
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 5 {
		t.Fatalf("Usage = %+v, want input 7 output 5", resp.Usage)
	}
	if resp.Metadata.Provider != "gemini" || resp.Metadata.Model != "gemini/gemini-2.0-flash" || resp.Metadata.Latency <= 0 {
		t.Fatalf("Metadata = %+v, want gemini model metadata", resp.Metadata)
	}
}

func TestGeminiPromptCacheNoOpMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "done"}]},
				"finishReason": "STOP"
			}]
		}`))
	}))
	defer server.Close()

	p, err := provider.NewGemini(provider.GeminiConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewGemini returned error: %v", err)
	}
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "gemini/gemini-2.0-flash",
		System:   "Stable harness prompt.",
		Messages: []provider.Message{provider.UserText("hello")},
		CacheKey: "prompt:test-cache-key",
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	assertPromptCacheNoOp(t, resp.Metadata.PromptCache)
}

func TestGeminiCompleteParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"text": "need lookup"},
						{"functionCall": {"name": "lookup_ticket", "args": {"id": "T-1"}}}
					]
				},
				"finishReason": "STOP"
			}]
		}`))
	}))
	defer server.Close()

	p, err := provider.NewGemini(provider.GeminiConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewGemini returned error: %v", err)
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "gemini/gemini-2.0-flash",
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
	if call.ID != "gemini_tool_0" || call.Name != "lookup_ticket" {
		t.Fatalf("ToolCall = %+v, want generated ID and lookup_ticket", call)
	}
	assertProviderJSONEqual(t, call.Arguments, `{"id":"T-1"}`)
}

func TestGeminiCompletePreservesMaxTokensWithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [{
						"functionCall": {"name": "publish", "args": {"value": "partial"}}
					}]
				},
				"finishReason": "MAX_TOKENS"
			}]
		}`))
	}))
	defer server.Close()

	p, err := provider.NewGemini(provider.GeminiConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewGemini returned error: %v", err)
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "gemini/gemini-2.0-flash",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.StopReason != provider.StopMaxTokens {
		t.Fatalf("StopReason = %q, want max_tokens", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want retained call for observability", len(resp.ToolCalls))
	}
}

func TestGeminiCompleteSendsToolResultMessages(t *testing.T) {
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
		assertNestedString(t, got, "model", "contents", 1, "role")
		assertNestedString(t, got, "lookup_ticket", "contents", 1, "parts", 0, "functionCall", "name")
		assertNestedString(t, got, "user", "contents", 2, "role")
		assertNestedString(t, got, "lookup_ticket", "contents", 2, "parts", 0, "functionResponse", "name")
		assertNestedString(t, got, "open", "contents", 2, "parts", 0, "functionResponse", "response", "status")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "ticket is open"}]},
				"finishReason": "STOP"
			}]
		}`))
	}))
	defer server.Close()

	p, err := provider.NewGemini(provider.GeminiConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewGemini returned error: %v", err)
	}

	_, err = p.Complete(context.Background(), provider.Request{
		Model: "gemini/gemini-2.0-flash",
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

func TestGeminiCompleteRejectsWrongProviderWithoutCallingServer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer server.Close()

	p, err := provider.NewGemini(provider.GeminiConfig{
		APIKey:  "test-key",
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewGemini returned error: %v", err)
	}

	_, err = p.Complete(context.Background(), provider.Request{
		Model:    "anthropic/claude-sonnet-4-6",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err == nil {
		t.Fatal("Complete returned nil error for wrong provider")
	}
	if calls != 0 {
		t.Fatalf("server calls = %d, want 0", calls)
	}
}

func assertNestedString(t *testing.T, value any, want string, path ...any) {
	t.Helper()
	got := nestedValue(t, value, path...)
	gotString, ok := got.(string)
	if !ok {
		t.Fatalf("%v = %T(%v), want string %q", path, got, got, want)
	}
	if gotString != want {
		t.Fatalf("%v = %q, want %q", path, gotString, want)
	}
}

func assertPromptCacheNoOp(t *testing.T, cache provider.PromptCacheMetadata) {
	t.Helper()
	if cache.CacheKey != "prompt:test-cache-key" ||
		!cache.Requested ||
		cache.Supported ||
		cache.Applied ||
		cache.Reason == "" {
		t.Fatalf("PromptCache = %+v, want explicit unsupported no-op metadata", cache)
	}
}

func nestedValue(t *testing.T, value any, path ...any) any {
	t.Helper()
	current := value
	for _, part := range path {
		switch key := part.(type) {
		case string:
			object, ok := current.(map[string]any)
			if !ok {
				t.Fatalf("path %v: current value is %T, want object", path, current)
			}
			current = object[key]
		case int:
			array, ok := current.([]any)
			if !ok {
				t.Fatalf("path %v: current value is %T, want array", path, current)
			}
			if key < 0 || key >= len(array) {
				t.Fatalf("path %v: index %d out of range", path, key)
			}
			current = array[key]
		default:
			t.Fatalf("unsupported path segment %T", part)
		}
	}
	return current
}

func assertProviderJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("unmarshal got JSON: %v", err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("unmarshal want JSON: %v", err)
	}
	if !providerJSONDeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}

func providerJSONDeepEqual(a, b any) bool {
	aBytes, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bBytes, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(aBytes) == string(bBytes)
}
