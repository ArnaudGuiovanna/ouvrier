package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

func (r geminiResponse) toProviderResponse() (Response, error) {
	resp := Response{
		Usage: Usage{
			InputTokens:  r.UsageMetadata.PromptTokenCount,
			OutputTokens: r.UsageMetadata.CandidatesTokenCount,
		},
	}
	if len(r.Candidates) == 0 {
		return resp, nil
	}
	candidate := r.Candidates[0]
	stopReason, err := geminiStopReason(candidate.FinishReason)
	if err != nil {
		return Response{}, err
	}
	resp.StopReason = stopReason
	var text []string
	for _, part := range candidate.Content.Parts {
		if part.Text != "" {
			text = append(text, part.Text)
		}
		if part.FunctionCall != nil {
			call := geminiProviderToolCall(part.FunctionCall, len(resp.ToolCalls))
			if err := call.Validate(); err != nil {
				return Response{}, err
			}
			resp.ToolCalls = append(resp.ToolCalls, call)
		}
	}
	resp.Text = strings.Join(text, "\n")
	if len(resp.ToolCalls) > 0 && resp.StopReason != StopMaxTokens {
		resp.StopReason = StopToolUse
	}
	return resp, nil
}

func geminiProviderToolCall(call *geminiFunctionCall, index int) ToolCall {
	id := call.ID
	if id == "" {
		id = fmt.Sprintf("gemini_tool_%d", index)
	}
	return ToolCall{
		ID:        id,
		Name:      call.Name,
		Arguments: geminiArguments(call.Args),
	}
}

// geminiStopReason maps the candidate finish reason onto the provider
// contract. Unknown reasons (SAFETY, RECITATION, future additions) fail
// closed with an error instead of passing through lowercased, so a blocked or
// exotic finish can never masquerade as a normal completion — or be silently
// rewritten to tool_use when function calls are present.
func geminiStopReason(reason string) (StopReason, error) {
	switch reason {
	case "", "STOP":
		return StopEndTurn, nil
	case "MAX_TOKENS":
		return StopMaxTokens, nil
	default:
		return "", fmt.Errorf("unsupported gemini finish reason %q", reason)
	}
}

func geminiArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), trimmed...)
}

func geminiToolResponsePayload(result *ToolResult) (json.RawMessage, error) {
	content := bytes.TrimSpace(result.Content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		content = []byte(`{}`)
	}
	if !json.Valid(content) {
		return nil, fmt.Errorf("tool result %q content must be valid JSON", result.Name)
	}

	var value any
	if err := json.Unmarshal(content, &value); err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		if result.IsError {
			object["is_error"] = true
		}
		return json.Marshal(object)
	}

	wrapped := map[string]any{"content": value}
	if result.IsError {
		wrapped["is_error"] = true
	}
	return json.Marshal(wrapped)
}

func geminiTextContent(role, text string) geminiContent {
	return geminiContent{Role: role, Parts: []geminiPart{{Text: text}}}
}
