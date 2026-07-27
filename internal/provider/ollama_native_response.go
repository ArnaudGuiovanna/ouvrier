package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	stopReason, err := ollamaNativeStopReason(r.DoneReason, len(resp.ToolCalls) > 0)
	if err != nil {
		return Response{}, err
	}
	resp.StopReason = stopReason
	return resp, nil
}

// ollamaNativeStopReason maps the native done_reason onto the provider
// contract. Unknown reasons (load, unload, future additions) fail closed with
// an error instead of defaulting to end_turn, so an abnormal finish can never
// masquerade as a normal completion.
func ollamaNativeStopReason(reason string, hasToolCalls bool) (StopReason, error) {
	switch reason {
	case "length", "max_tokens":
		return StopMaxTokens, nil
	case "", "stop":
		if hasToolCalls {
			return StopToolUse, nil
		}
		return StopEndTurn, nil
	default:
		return "", fmt.Errorf("unsupported ollama done reason %q", reason)
	}
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
