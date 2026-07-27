package provider

import (
	"bytes"
	"encoding/json"
	"strconv"
)

func (r ollamaNativeResponse) toProviderResponse() (Response, error) {
	resp := Response{
		Text: r.Message.Content,
		Usage: Usage{
			InputTokens:  r.PromptEvalCount,
			OutputTokens: r.EvalCount,
		},
	}
	for _, raw := range r.Message.ToolCalls {
		call := ToolCall{
			ID:        "ollama_tool_" + strconv.Itoa(len(resp.ToolCalls)),
			Name:      raw.Function.Name,
			Arguments: ollamaNativeArguments(raw.Function.Arguments),
		}
		if err := call.Validate(); err != nil {
			return Response{}, err
		}
		resp.ToolCalls = append(resp.ToolCalls, call)
	}
	resp.StopReason = ollamaNativeStopReason(r.DoneReason, len(resp.ToolCalls) > 0)
	return resp, nil
}

func ollamaNativeStopReason(reason string, hasToolCalls bool) StopReason {
	if reason == "length" || reason == "max_tokens" {
		return StopMaxTokens
	}
	if hasToolCalls {
		return StopToolUse
	}
	return StopEndTurn
}

func ollamaNativeArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), trimmed...)
}

func ollamaNativeToolContent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text
	}
	return string(trimmed)
}
