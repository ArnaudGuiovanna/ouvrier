package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/provider"
)

type compatProvider interface {
	provider.Provider
	BaseURL() string
}

type compatProviderFactory func(t *testing.T, baseURL string) compatProvider

type compatChatRequest struct {
	Model    string              `json:"model"`
	Messages []compatChatMessage `json:"messages"`
	Tools    []compatTool        `json:"tools"`
}

type compatChatMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content"`
	ToolCalls  []compatToolCall `json:"tool_calls"`
	ToolCallID string           `json:"tool_call_id"`
}

type compatToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function compatToolFunction `json:"function"`
}

type compatTool struct {
	Type     string             `json:"type"`
	Function compatToolFunction `json:"function"`
}

type compatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Arguments   string          `json:"arguments"`
}

func runCompatChatCompletionTest(t *testing.T, providerName, modelName, wantAuth string, factory compatProviderFactory) {
	t.Helper()

	var got compatChatRequest
	var gotMethod string
	var gotPath string
	var gotAuth string
	var gotContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"chatcmpl_test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7}
		}`)
	}))
	defer server.Close()

	p := factory(t, server.URL+"/")
	call := provider.ToolCall{
		ID:        "call_1",
		Name:      "lookup_weather",
		Arguments: json.RawMessage(`{"city":"Paris"}`),
	}

	resp, err := p.Complete(context.Background(), provider.Request{
		Model:  providerName + "/" + modelName,
		System: "Be concise.",
		Messages: []provider.Message{
			provider.UserText("hello"),
			provider.AssistantToolCalls("checking", call),
			provider.ToolResultText(call, "sunny", false),
		},
		Tools: []provider.ToolSpec{{
			Name:        "lookup_weather",
			Description: "Look up weather.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
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
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("Usage = %+v, want prompt/completion tokens", resp.Usage)
	}
	if resp.Metadata.Provider != providerName || resp.Metadata.Model != providerName+"/"+modelName || resp.Metadata.Latency <= 0 {
		t.Fatalf("Metadata = %+v, want provider/model/latency", resp.Metadata)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != wantAuth {
		t.Fatalf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
	if !strings.HasPrefix(gotContentType, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if got.Model != modelName {
		t.Fatalf("model = %q, want unprefixed model %q", got.Model, modelName)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(got.Messages), got.Messages)
	}
	assertMessageContent(t, got.Messages[0], "system", "Be concise.")
	assertMessageContent(t, got.Messages[1], "user", "hello")
	assertMessageContent(t, got.Messages[2], "assistant", "checking")
	if len(got.Messages[2].ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %d, want 1", len(got.Messages[2].ToolCalls))
	}
	gotCall := got.Messages[2].ToolCalls[0]
	if gotCall.ID != "call_1" || gotCall.Type != "function" || gotCall.Function.Name != "lookup_weather" {
		t.Fatalf("assistant tool call = %+v, want function lookup_weather call_1", gotCall)
	}
	if gotCall.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("assistant tool arguments = %q", gotCall.Function.Arguments)
	}
	assertMessageContent(t, got.Messages[3], "tool", "sunny")
	if got.Messages[3].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id = %q, want call_1", got.Messages[3].ToolCallID)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(got.Tools))
	}
	gotTool := got.Tools[0]
	if gotTool.Type != "function" || gotTool.Function.Name != "lookup_weather" || gotTool.Function.Description != "Look up weather." {
		t.Fatalf("tool = %+v, want lookup_weather function", gotTool)
	}
	assertJSONEqual(t, gotTool.Function.Parameters, `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)
}

func TestOpenAICompatPromptCacheNoOpMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"chatcmpl_test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7}
		}`)
	}))
	defer server.Close()

	p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewOpenAI returned error: %v", err)
	}
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "openai/gpt-4.1-mini",
		System:   "Stable harness prompt.",
		Messages: []provider.Message{provider.UserText("hello")},
		CacheKey: "prompt:test-cache-key",
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	cache := resp.Metadata.PromptCache
	if cache.CacheKey != "prompt:test-cache-key" ||
		!cache.Requested ||
		cache.Supported ||
		cache.Applied ||
		cache.Reason == "" {
		t.Fatalf("PromptCache = %+v, want explicit unsupported no-op metadata", cache)
	}
}

func runCompatToolCallResponseTest(t *testing.T, providerName, modelName string, factory compatProviderFactory) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"chatcmpl_test",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"need lookup",
					"tool_calls":[{
						"id":"call_9",
						"type":"function",
						"function":{"name":"lookup_ticket","arguments":"{\"ticket_id\":\"T-1\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":3}
		}`)
	}))
	defer server.Close()

	p := factory(t, server.URL)
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    providerName + "/" + modelName,
		Messages: []provider.Message{provider.UserText("triage T-1")},
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
	if call.ID != "call_9" || call.Name != "lookup_ticket" {
		t.Fatalf("ToolCall = %+v, want lookup_ticket call_9", call)
	}
	assertJSONEqual(t, call.Arguments, `{"ticket_id":"T-1"}`)
}

func runCompatForeignModelTest(t *testing.T, providerName string, factory compatProviderFactory) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not be called for a foreign model")
	}))
	defer server.Close()

	p := factory(t, server.URL)
	_, err := p.Complete(context.Background(), provider.Request{
		Model:    "other/model",
		Messages: []provider.Message{provider.UserText("hello")},
	})
	if err == nil {
		t.Fatalf("Complete returned nil error for foreign model")
	}
	if !strings.Contains(err.Error(), providerName+" provider cannot run model") {
		t.Fatalf("error = %v, want provider mismatch", err)
	}
}

func assertMessageContent(t *testing.T, msg compatChatMessage, role, content string) {
	t.Helper()
	if msg.Role != role {
		t.Fatalf("message role = %q, want %q", msg.Role, role)
	}
	if msg.Content == nil {
		t.Fatalf("%s message content is nil, want %q", role, content)
	}
	if *msg.Content != content {
		t.Fatalf("%s message content = %q, want %q", role, *msg.Content, content)
	}
}

func assertJSONEqual(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(raw, &gotValue); err != nil {
		t.Fatalf("decode got JSON %q: %v", string(raw), err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want JSON %q: %v", want, err)
	}
	if fmt.Sprint(gotValue) != fmt.Sprint(wantValue) {
		t.Fatalf("JSON = %s, want %s", string(raw), want)
	}
}
