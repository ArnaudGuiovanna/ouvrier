package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (r openAICompatResponse) toProviderResponse() (Response, error) {
	if len(r.Choices) == 0 {
		return Response{}, errors.New("response has no choices")
	}

	choice := r.Choices[0]
	text, err := openAICompatResponseText(choice.Message.Content)
	if err != nil {
		return Response{}, err
	}
	calls, err := openAICompatProviderToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return Response{}, err
	}
	stopReason, err := openAICompatStopReason(choice.FinishReason, len(calls) > 0)
	if err != nil {
		return Response{}, err
	}

	return Response{
		Text:       text,
		ToolCalls:  calls,
		StopReason: stopReason,
		Usage: Usage{
			InputTokens:  r.Usage.PromptTokens,
			OutputTokens: r.Usage.CompletionTokens,
		},
	}, nil
}

func openAICompatResponseText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	if !json.Valid(raw) {
		return "", errors.New("assistant content must be valid JSON")
	}
	return string(raw), nil
}

func openAICompatProviderToolCalls(calls []openAICompatToolCall) ([]ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		if call.Type != "" && call.Type != "function" {
			return nil, fmt.Errorf("unsupported tool call type %q", call.Type)
		}
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		converted := ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: json.RawMessage(args),
		}
		if err := converted.Validate(); err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func openAICompatStopReason(reason string, hasToolCalls bool) (StopReason, error) {
	switch reason {
	case "", "stop":
		if hasToolCalls {
			return StopToolUse, nil
		}
		return StopEndTurn, nil
	case "tool_calls", "function_call":
		return StopToolUse, nil
	case "length", "model_length":
		return StopMaxTokens, nil
	default:
		return "", fmt.Errorf("unsupported finish reason %q", reason)
	}
}
