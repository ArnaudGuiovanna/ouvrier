package provider_test

// Provider stop-reason conformance suite (slice 0A.2).
//
// Every adapter is run against the SAME contract cases via an httptest server
// speaking that adapter's wire format. The contract under test:
//
//   - stop-reason mapping: end-turn, tool-use, max-tokens, and max-tokens with
//     tool calls present (tool calls are retained for observability but the
//     stop reason must stay StopMaxTokens so the harness never executes them);
//   - tool-call parsing (id, name, JSON arguments);
//   - usage propagation (input/output tokens);
//   - unknown/unsupported stop reasons fail closed with a deterministic error
//     ("unsupported ... reason") — never a silent success that the harness
//     would treat as a normal completion;
//   - streaming paths (anthropic SSE, openai-compatible SSE) map stop reasons
//     and usage identically to the non-streaming paths.
//
// Documented unknown-stop-reason behavior per adapter (all deterministic
// errors after 0A.2):
//   - anthropic:      "", end_turn, stop_sequence -> end_turn; tool_use;
//     max_tokens; anything else (pause_turn, refusal, ...) -> error
//   - openai-compat (openai, azure, deepseek, groq, mistral, vllm):
//     "", stop -> end_turn/tool_use; tool_calls, function_call -> tool_use;
//     length, model_length -> max_tokens; anything else (content_filter, ...)
//     -> error
//   - gemini:         "", STOP -> end_turn (tool_use when calls present);
//     MAX_TOKENS -> max_tokens; anything else (SAFETY, RECITATION, ...) -> error
//   - ollama (native): "", stop -> end_turn/tool_use; length, max_tokens ->
//     max_tokens; anything else -> error
//   - bedrock:        "", end_turn, stop_sequence -> end_turn/tool_use;
//     tool_use; max_tokens; anything else (guardrail_intervened, ...) -> error

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// Conformance case names. Each adapter provides one wire-format fixture per
// case; the assertions are shared.
const (
	caseEndTurn               = "end_turn"
	caseToolUse               = "tool_use"
	caseMaxTokens             = "max_tokens"
	caseMaxTokensWithToolCall = "max_tokens_with_tool_calls"
	caseUnknownStopReason     = "unknown_stop_reason"
)

var conformanceCaseNames = []string{
	caseEndTurn,
	caseToolUse,
	caseMaxTokens,
	caseMaxTokensWithToolCall,
	caseUnknownStopReason,
}

type conformanceAdapter struct {
	name  string
	model string
	// wantToolID is the expected ID of the first parsed tool call. Adapters
	// whose wire format has no tool-call id synthesize a deterministic one.
	wantToolID  string
	streaming   bool
	contentType string
	fixtures    map[string]string
	newProvider func(t *testing.T, baseURL string) provider.Provider
}

func (a conformanceAdapter) invoke(ctx context.Context, p provider.Provider, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	if !a.streaming {
		return p.Complete(ctx, req)
	}
	streamer, ok := p.(provider.StreamingProvider)
	if !ok {
		return provider.Response{}, nil
	}
	return streamer.CompleteStream(ctx, req, onDelta)
}

func TestProviderStopReasonConformance(t *testing.T) {
	for _, adapter := range conformanceAdapters() {
		adapter := adapter
		t.Run(adapter.name, func(t *testing.T) {
			for _, caseName := range conformanceCaseNames {
				caseName := caseName
				t.Run(caseName, func(t *testing.T) {
					runConformanceCase(t, adapter, caseName)
				})
			}
		})
	}
}

func runConformanceCase(t *testing.T, adapter conformanceAdapter, caseName string) {
	t.Helper()
	body, ok := adapter.fixtures[caseName]
	if !ok {
		t.Fatalf("adapter %q has no fixture for case %q", adapter.name, caseName)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", adapter.contentType)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	p := adapter.newProvider(t, server.URL)
	if adapter.streaming {
		if _, ok := p.(provider.StreamingProvider); !ok {
			t.Fatalf("adapter %q does not implement StreamingProvider", adapter.name)
		}
	}

	var deltas []string
	resp, err := adapter.invoke(context.Background(), p, provider.Request{
		Model:    adapter.model,
		Messages: []provider.Message{provider.UserText("hello")},
	}, func(d provider.Delta) {
		deltas = append(deltas, d.Text)
	})

	switch caseName {
	case caseEndTurn:
		requireConformanceOK(t, err)
		assertStopReason(t, resp, provider.StopEndTurn)
		if resp.Text != "done" {
			t.Fatalf("Text = %q, want done", resp.Text)
		}
		if len(resp.ToolCalls) != 0 {
			t.Fatalf("ToolCalls = %d, want 0", len(resp.ToolCalls))
		}
		assertUsage(t, resp, 7, 5)
	case caseToolUse:
		requireConformanceOK(t, err)
		assertStopReason(t, resp, provider.StopToolUse)
		if resp.Text != "need lookup" {
			t.Fatalf("Text = %q, want need lookup", resp.Text)
		}
		assertSingleToolCall(t, resp, adapter.wantToolID, "lookup_ticket", `{"id":"T-1"}`)
		assertUsage(t, resp, 9, 4)
	case caseMaxTokens:
		requireConformanceOK(t, err)
		assertStopReason(t, resp, provider.StopMaxTokens)
		if resp.Text != "partial" {
			t.Fatalf("Text = %q, want partial", resp.Text)
		}
		if len(resp.ToolCalls) != 0 {
			t.Fatalf("ToolCalls = %d, want 0", len(resp.ToolCalls))
		}
		assertUsage(t, resp, 3, 8)
	case caseMaxTokensWithToolCall:
		requireConformanceOK(t, err)
		// Tool calls present in a truncated turn must be retained for
		// observability, but the stop reason must stay max_tokens — the
		// harness relies on this to fail closed and never execute them.
		assertStopReason(t, resp, provider.StopMaxTokens)
		assertSingleToolCall(t, resp, adapter.wantToolID, "publish", `{"value":"partial"}`)
		assertUsage(t, resp, 2, 6)
	case caseUnknownStopReason:
		if err == nil {
			t.Fatalf("unknown stop reason returned nil error, response = %+v (silent success is forbidden)", resp)
		}
		if !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("error = %v, want deterministic unsupported-stop-reason error", err)
		}
		return
	default:
		t.Fatalf("unhandled conformance case %q", caseName)
	}

	if adapter.streaming {
		if joined := strings.Join(deltas, ""); joined != resp.Text {
			t.Fatalf("streamed deltas = %q, want assembled text %q", joined, resp.Text)
		}
	}
}

func requireConformanceOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
}

func assertStopReason(t *testing.T, resp provider.Response, want provider.StopReason) {
	t.Helper()
	if resp.StopReason != want {
		t.Fatalf("StopReason = %q, want %q", resp.StopReason, want)
	}
}

func assertUsage(t *testing.T, resp provider.Response, wantInput, wantOutput int) {
	t.Helper()
	if resp.Usage.InputTokens != wantInput || resp.Usage.OutputTokens != wantOutput {
		t.Fatalf("Usage = %+v, want input %d output %d", resp.Usage, wantInput, wantOutput)
	}
}

func assertSingleToolCall(t *testing.T, resp provider.Response, wantID, wantName, wantArgs string) {
	t.Helper()
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1 retained call", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != wantID {
		t.Fatalf("ToolCall.ID = %q, want %q", call.ID, wantID)
	}
	if call.Name != wantName {
		t.Fatalf("ToolCall.Name = %q, want %q", call.Name, wantName)
	}
	assertProviderJSONEqual(t, call.Arguments, wantArgs)
}

func conformanceAdapters() []conformanceAdapter {
	adapters := []conformanceAdapter{
		{
			name:        "anthropic",
			model:       "anthropic/claude-sonnet-4-6",
			wantToolID:  "call_1",
			contentType: "application/json",
			fixtures:    anthropicConformanceFixtures,
			newProvider: func(t *testing.T, baseURL string) provider.Provider {
				p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key", BaseURL: baseURL})
				if err != nil {
					t.Fatalf("NewAnthropic returned error: %v", err)
				}
				return p
			},
		},
		{
			name:        "anthropic_stream",
			model:       "anthropic/claude-sonnet-4-6",
			wantToolID:  "call_1",
			streaming:   true,
			contentType: "text/event-stream",
			fixtures:    anthropicStreamConformanceFixtures,
			newProvider: func(t *testing.T, baseURL string) provider.Provider {
				p, err := provider.NewAnthropic(provider.AnthropicConfig{APIKey: "test-key", BaseURL: baseURL})
				if err != nil {
					t.Fatalf("NewAnthropic returned error: %v", err)
				}
				return p
			},
		},
		{
			name:        "bedrock",
			model:       "bedrock/anthropic.claude-3-sonnet-20240229-v1:0",
			wantToolID:  "call_1",
			contentType: "application/json",
			fixtures:    bedrockConformanceFixtures,
			newProvider: func(t *testing.T, baseURL string) provider.Provider {
				p, err := provider.NewBedrock(provider.BedrockConfig{
					AccessKeyID:     "AKID",
					SecretAccessKey: "secret",
					Region:          "us-east-1",
					BaseURL:         baseURL,
				})
				if err != nil {
					t.Fatalf("NewBedrock returned error: %v", err)
				}
				return p
			},
		},
		{
			name:        "gemini",
			model:       "gemini/gemini-2.0-flash",
			wantToolID:  "gemini_tool_0",
			contentType: "application/json",
			fixtures:    geminiConformanceFixtures,
			newProvider: func(t *testing.T, baseURL string) provider.Provider {
				p, err := provider.NewGemini(provider.GeminiConfig{APIKey: "test-key", BaseURL: baseURL})
				if err != nil {
					t.Fatalf("NewGemini returned error: %v", err)
				}
				return p
			},
		},
		{
			name:        "ollama",
			model:       "ollama/llama3.1",
			wantToolID:  "ollama_tool_0",
			contentType: "application/json",
			fixtures:    ollamaConformanceFixtures,
			newProvider: func(t *testing.T, baseURL string) provider.Provider {
				return provider.NewOllama(provider.OllamaConfig{BaseURL: baseURL})
			},
		},
		{
			name:        "openai_stream",
			model:       "openai/gpt-4o-mini",
			wantToolID:  "call_1",
			streaming:   true,
			contentType: "text/event-stream",
			fixtures:    openAICompatStreamConformanceFixtures,
			newProvider: func(t *testing.T, baseURL string) provider.Provider {
				p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: baseURL})
				if err != nil {
					t.Fatalf("NewOpenAI returned error: %v", err)
				}
				return p
			},
		},
	}
	return append(adapters, openAICompatConformanceAdapters()...)
}

// openAICompatConformanceAdapters covers every provider served by the shared
// openai_compat layer. They speak the same wire format, so they share one
// fixture set; running each constructor proves the whole family routes through
// the same conformant mapping.
func openAICompatConformanceAdapters() []conformanceAdapter {
	factories := []struct {
		name    string
		model   string
		factory func(t *testing.T, baseURL string) provider.Provider
	}{
		{"openai", "openai/gpt-4o-mini", func(t *testing.T, baseURL string) provider.Provider {
			p, err := provider.NewOpenAI(provider.OpenAIConfig{APIKey: "test-key", BaseURL: baseURL})
			if err != nil {
				t.Fatalf("NewOpenAI returned error: %v", err)
			}
			return p
		}},
		{"azure", "azure/gpt-4o", func(t *testing.T, baseURL string) provider.Provider {
			p, err := provider.NewAzureOpenAI(provider.AzureOpenAIConfig{APIKey: "test-key", BaseURL: baseURL})
			if err != nil {
				t.Fatalf("NewAzureOpenAI returned error: %v", err)
			}
			return p
		}},
		{"deepseek", "deepseek/deepseek-chat", func(t *testing.T, baseURL string) provider.Provider {
			p, err := provider.NewDeepSeek(provider.DeepSeekConfig{APIKey: "test-key", BaseURL: baseURL})
			if err != nil {
				t.Fatalf("NewDeepSeek returned error: %v", err)
			}
			return p
		}},
		{"groq", "groq/llama-3.1-70b", func(t *testing.T, baseURL string) provider.Provider {
			p, err := provider.NewGroq(provider.GroqConfig{APIKey: "test-key", BaseURL: baseURL})
			if err != nil {
				t.Fatalf("NewGroq returned error: %v", err)
			}
			return p
		}},
		{"mistral", "mistral/mistral-large", func(t *testing.T, baseURL string) provider.Provider {
			p, err := provider.NewMistral(provider.MistralConfig{APIKey: "test-key", BaseURL: baseURL})
			if err != nil {
				t.Fatalf("NewMistral returned error: %v", err)
			}
			return p
		}},
		{"vllm", "vllm/qwen2.5", func(t *testing.T, baseURL string) provider.Provider {
			p, err := provider.NewVLLM(provider.VLLMConfig{BaseURL: baseURL})
			if err != nil {
				t.Fatalf("NewVLLM returned error: %v", err)
			}
			return p
		}},
	}
	adapters := make([]conformanceAdapter, 0, len(factories))
	for _, f := range factories {
		adapters = append(adapters, conformanceAdapter{
			name:        f.name,
			model:       f.model,
			wantToolID:  "call_1",
			contentType: "application/json",
			fixtures:    openAICompatConformanceFixtures,
			newProvider: f.factory,
		})
	}
	return adapters
}

var anthropicConformanceFixtures = map[string]string{
	caseEndTurn: `{
		"content":[{"type":"text","text":"done"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":7,"output_tokens":5}
	}`,
	caseToolUse: `{
		"content":[
			{"type":"text","text":"need lookup"},
			{"type":"tool_use","id":"call_1","name":"lookup_ticket","input":{"id":"T-1"}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":9,"output_tokens":4}
	}`,
	caseMaxTokens: `{
		"content":[{"type":"text","text":"partial"}],
		"stop_reason":"max_tokens",
		"usage":{"input_tokens":3,"output_tokens":8}
	}`,
	caseMaxTokensWithToolCall: `{
		"content":[{"type":"tool_use","id":"call_1","name":"publish","input":{"value":"partial"}}],
		"stop_reason":"max_tokens",
		"usage":{"input_tokens":2,"output_tokens":6}
	}`,
	caseUnknownStopReason: `{
		"content":[{"type":"text","text":"?"}],
		"stop_reason":"pause_turn",
		"usage":{"input_tokens":1,"output_tokens":1}
	}`,
}

var anthropicStreamConformanceFixtures = map[string]string{
	caseEndTurn: "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"do"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ne"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n",
	caseToolUse: "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":9,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"need "}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lookup"}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"lookup_ticket"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"T-1\"}"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n",
	caseMaxTokens: "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":8}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n",
	caseMaxTokensWithToolCall: "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":2,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"publish"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"value\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"partial\"}"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":6}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n",
	caseUnknownStopReason: "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n",
}

var openAICompatConformanceFixtures = map[string]string{
	caseEndTurn: `{
		"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":7,"completion_tokens":5}
	}`,
	caseToolUse: `{
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"content":"need lookup",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup_ticket","arguments":"{\"id\":\"T-1\"}"}}]
			},
			"finish_reason":"tool_calls"
		}],
		"usage":{"prompt_tokens":9,"completion_tokens":4}
	}`,
	caseMaxTokens: `{
		"choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],
		"usage":{"prompt_tokens":3,"completion_tokens":8}
	}`,
	caseMaxTokensWithToolCall: `{
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"content":null,
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"publish","arguments":"{\"value\":\"partial\"}"}}]
			},
			"finish_reason":"length"
		}],
		"usage":{"prompt_tokens":2,"completion_tokens":6}
	}`,
	caseUnknownStopReason: `{
		"choices":[{"index":0,"message":{"role":"assistant","content":"?"},"finish_reason":"content_filter"}],
		"usage":{"prompt_tokens":1,"completion_tokens":1}
	}`,
}

var openAICompatStreamConformanceFixtures = map[string]string{
	caseEndTurn: `data: {"choices":[{"delta":{"content":"do"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"ne"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}` + "\n\n" +
		"data: [DONE]\n\n",
	caseToolUse: `data: {"choices":[{"delta":{"content":"need lookup"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup_ticket","arguments":"{\"id\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"T-1\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}` + "\n\n" +
		"data: [DONE]\n\n",
	caseMaxTokens: `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":8}}` + "\n\n" +
		"data: [DONE]\n\n",
	caseMaxTokensWithToolCall: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"publish","arguments":"{\"value\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"partial\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":2,"completion_tokens":6}}` + "\n\n" +
		"data: [DONE]\n\n",
	caseUnknownStopReason: `data: {"choices":[{"delta":{"content":"?"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
		"data: [DONE]\n\n",
}

var geminiConformanceFixtures = map[string]string{
	caseEndTurn: `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":5}
	}`,
	caseToolUse: `{
		"candidates":[{
			"content":{"role":"model","parts":[
				{"text":"need lookup"},
				{"functionCall":{"name":"lookup_ticket","args":{"id":"T-1"}}}
			]},
			"finishReason":"STOP"
		}],
		"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":4}
	}`,
	caseMaxTokens: `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":8}
	}`,
	caseMaxTokensWithToolCall: `{
		"candidates":[{
			"content":{"role":"model","parts":[{"functionCall":{"name":"publish","args":{"value":"partial"}}}]},
			"finishReason":"MAX_TOKENS"
		}],
		"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":6}
	}`,
	caseUnknownStopReason: `{
		"candidates":[{"content":{"role":"model","parts":[{"text":"?"}]},"finishReason":"SAFETY"}],
		"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}
	}`,
}

var ollamaConformanceFixtures = map[string]string{
	caseEndTurn: `{
		"message":{"role":"assistant","content":"done"},
		"done":true,"done_reason":"stop",
		"prompt_eval_count":7,"eval_count":5
	}`,
	caseToolUse: `{
		"message":{
			"role":"assistant",
			"content":"need lookup",
			"tool_calls":[{"function":{"name":"lookup_ticket","arguments":{"id":"T-1"}}}]
		},
		"done":true,"done_reason":"stop",
		"prompt_eval_count":9,"eval_count":4
	}`,
	caseMaxTokens: `{
		"message":{"role":"assistant","content":"partial"},
		"done":true,"done_reason":"length",
		"prompt_eval_count":3,"eval_count":8
	}`,
	caseMaxTokensWithToolCall: `{
		"message":{
			"role":"assistant",
			"content":"",
			"tool_calls":[{"function":{"name":"publish","arguments":{"value":"partial"}}}]
		},
		"done":true,"done_reason":"length",
		"prompt_eval_count":2,"eval_count":6
	}`,
	caseUnknownStopReason: `{
		"message":{"role":"assistant","content":"?"},
		"done":true,"done_reason":"abort",
		"prompt_eval_count":1,"eval_count":1
	}`,
}

var bedrockConformanceFixtures = map[string]string{
	caseEndTurn: `{
		"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":7,"outputTokens":5}
	}`,
	caseToolUse: `{
		"output":{"message":{"role":"assistant","content":[
			{"text":"need lookup"},
			{"toolUse":{"toolUseId":"call_1","name":"lookup_ticket","input":{"id":"T-1"}}}
		]}},
		"stopReason":"tool_use",
		"usage":{"inputTokens":9,"outputTokens":4}
	}`,
	caseMaxTokens: `{
		"output":{"message":{"role":"assistant","content":[{"text":"partial"}]}},
		"stopReason":"max_tokens",
		"usage":{"inputTokens":3,"outputTokens":8}
	}`,
	caseMaxTokensWithToolCall: `{
		"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"call_1","name":"publish","input":{"value":"partial"}}}
		]}},
		"stopReason":"max_tokens",
		"usage":{"inputTokens":2,"outputTokens":6}
	}`,
	caseUnknownStopReason: `{
		"output":{"message":{"role":"assistant","content":[{"text":"?"}]}},
		"stopReason":"guardrail_intervened",
		"usage":{"inputTokens":1,"outputTokens":1}
	}`,
}
