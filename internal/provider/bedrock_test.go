package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeDoer struct {
	gotReq  *http.Request
	gotBody []byte
	resp    *http.Response
	err     error
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.gotReq = req
	if req.Body != nil {
		d.gotBody, _ = io.ReadAll(req.Body)
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.resp, nil
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestNewBedrockRequiresCredentialsAndRegion(t *testing.T) {
	if _, err := NewBedrock(BedrockConfig{Region: "us-east-1", SecretAccessKey: "s"}); err == nil {
		t.Fatal("NewBedrock returned nil error without access key")
	}
	if _, err := NewBedrock(BedrockConfig{Region: "us-east-1", AccessKeyID: "a"}); err == nil {
		t.Fatal("NewBedrock returned nil error without secret")
	}
	if _, err := NewBedrock(BedrockConfig{AccessKeyID: "a", SecretAccessKey: "s"}); err == nil {
		t.Fatal("NewBedrock returned nil error without region")
	}
	p, err := NewBedrock(BedrockConfig{AccessKeyID: "a", SecretAccessKey: "s", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewBedrock returned error: %v", err)
	}
	if p.Name() != "bedrock" {
		t.Fatalf("Name = %q, want bedrock", p.Name())
	}
}

func TestBedrockCompleteMapsConverseRequestAndSignsIt(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, `{
		"output":{"message":{"role":"assistant","content":[{"text":"done"}]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":11,"outputTokens":7}
	}`)}

	p, err := NewBedrock(BedrockConfig{
		AccessKeyID:     "AKID",
		SecretAccessKey: "secret",
		Region:          "us-east-1",
		Doer:            doer,
	})
	if err != nil {
		t.Fatalf("NewBedrock returned error: %v", err)
	}

	call := ToolCall{ID: "tool_1", Name: "lookup_weather", Arguments: json.RawMessage(`{"city":"Paris"}`)}
	resp, err := p.Complete(context.Background(), Request{
		Model:  "bedrock/anthropic.claude-3-5-sonnet-20240620-v1:0",
		System: "Be concise.",
		Messages: []Message{
			UserText("hello"),
			AssistantToolCalls("checking", call),
			ToolResultText(call, "sunny", false),
		},
		Tools: []ToolSpec{{
			Name:        "lookup_weather",
			Description: "Look up weather.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text != "done" || resp.StopReason != StopEndTurn {
		t.Fatalf("resp = %+v, want done/end_turn", resp)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if resp.Metadata.Provider != "bedrock" {
		t.Fatalf("metadata provider = %q", resp.Metadata.Provider)
	}

	// URL targets the Converse endpoint for the region + model id.
	wantURL := "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20240620-v1:0/converse"
	if doer.gotReq.URL.String() != wantURL {
		t.Fatalf("url = %q, want %q", doer.gotReq.URL.String(), wantURL)
	}
	if doer.gotReq.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", doer.gotReq.Method)
	}
	if !strings.HasPrefix(doer.gotReq.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		t.Fatalf("missing SigV4 Authorization: %q", doer.gotReq.Header.Get("Authorization"))
	}
	if doer.gotReq.Header.Get("X-Amz-Date") == "" {
		t.Fatal("missing X-Amz-Date")
	}

	var body bedrockConverseRequest
	if err := json.Unmarshal(doer.gotBody, &body); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if len(body.System) != 1 || body.System[0].Text != "Be concise." {
		t.Fatalf("system = %+v", body.System)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(body.Messages))
	}
	if body.Messages[0].Role != "user" || body.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("user message = %+v", body.Messages[0])
	}
	// assistant message has text + toolUse blocks.
	assistant := body.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 2 {
		t.Fatalf("assistant message = %+v", assistant)
	}
	if assistant.Content[1].ToolUse == nil || assistant.Content[1].ToolUse.Name != "lookup_weather" {
		t.Fatalf("assistant toolUse = %+v", assistant.Content[1])
	}
	if assistant.Content[1].ToolUse.ToolUseID != "tool_1" {
		t.Fatalf("toolUseId = %q", assistant.Content[1].ToolUse.ToolUseID)
	}
	// tool result message.
	toolMsg := body.Messages[2]
	if toolMsg.Role != "user" || toolMsg.Content[0].ToolResult == nil {
		t.Fatalf("tool result message = %+v", toolMsg)
	}
	if toolMsg.Content[0].ToolResult.ToolUseID != "tool_1" {
		t.Fatalf("tool result id = %q", toolMsg.Content[0].ToolResult.ToolUseID)
	}
	if body.ToolConfig == nil || len(body.ToolConfig.Tools) != 1 {
		t.Fatalf("toolConfig = %+v", body.ToolConfig)
	}
	if body.ToolConfig.Tools[0].ToolSpec.Name != "lookup_weather" {
		t.Fatalf("tool spec = %+v", body.ToolConfig.Tools[0])
	}
}

func TestBedrockCompleteParsesToolUseResponse(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, `{
		"output":{"message":{"role":"assistant","content":[
			{"text":"need lookup"},
			{"toolUse":{"toolUseId":"tu_9","name":"lookup_ticket","input":{"ticket_id":"T-1"}}}
		]}},
		"stopReason":"tool_use",
		"usage":{"inputTokens":5,"outputTokens":3}
	}`)}

	p, _ := NewBedrock(BedrockConfig{AccessKeyID: "a", SecretAccessKey: "s", Region: "us-east-1", Doer: doer})
	resp, err := p.Complete(context.Background(), Request{
		Model:    "bedrock/anthropic.claude",
		Messages: []Message{UserText("triage T-1")},
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if resp.Text != "need lookup" || resp.StopReason != StopToolUse {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	c := resp.ToolCalls[0]
	if c.ID != "tu_9" || c.Name != "lookup_ticket" {
		t.Fatalf("tool call = %+v", c)
	}
	var args map[string]any
	if err := json.Unmarshal(c.Arguments, &args); err != nil || args["ticket_id"] != "T-1" {
		t.Fatalf("args = %s err=%v", c.Arguments, err)
	}
}

func TestBedrockCompleteClassifiesStatusErrors(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(http.StatusTooManyRequests, `{"message":"slow down"}`)}
	p, _ := NewBedrock(BedrockConfig{AccessKeyID: "a", SecretAccessKey: "s", Region: "us-east-1", Doer: doer})
	_, err := p.Complete(context.Background(), Request{
		Model:    "bedrock/anthropic.claude",
		Messages: []Message{UserText("hi")},
	})
	if err == nil {
		t.Fatal("Complete returned nil error for 429")
	}
	if !IsTransientError(err) {
		t.Fatalf("429 should classify as retryable, got %v", err)
	}
	var classified ClassifiedError
	if !errors.As(err, &classified) || classified.Kind != ErrorRateLimit {
		t.Fatalf("error kind = %v, want rate_limit", err)
	}
}

func TestBedrockCompleteRejectsForeignModel(t *testing.T) {
	p, _ := NewBedrock(BedrockConfig{AccessKeyID: "a", SecretAccessKey: "s", Region: "us-east-1", Doer: &fakeDoer{}})
	_, err := p.Complete(context.Background(), Request{
		Model:    "other/model",
		Messages: []Message{UserText("hi")},
	})
	if err == nil || !strings.Contains(err.Error(), "bedrock provider cannot run model") {
		t.Fatalf("error = %v, want provider mismatch", err)
	}
}
