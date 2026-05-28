package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

func (r bedrockConverseResponse) toProviderResponse() (Response, error) {
	var parts []string
	var calls []ToolCall
	for _, block := range r.Output.Message.Content {
		switch {
		case block.ToolUse != nil:
			input := block.ToolUse.Input
			if len(strings.TrimSpace(string(input))) == 0 {
				input = json.RawMessage("{}")
			}
			call := ToolCall{
				ID:        block.ToolUse.ToolUseID,
				Name:      block.ToolUse.Name,
				Arguments: append(json.RawMessage(nil), input...),
			}
			if err := call.Validate(); err != nil {
				return Response{}, err
			}
			calls = append(calls, call)
		case block.Text != "":
			parts = append(parts, block.Text)
		}
	}

	stopReason, err := bedrockStopReason(r.StopReason, len(calls) > 0)
	if err != nil {
		return Response{}, err
	}

	return Response{
		Text:       strings.Join(parts, "\n"),
		ToolCalls:  calls,
		StopReason: stopReason,
		Usage: Usage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
		},
	}, nil
}

func bedrockStopReason(reason string, hasToolCalls bool) (StopReason, error) {
	switch reason {
	case "", "end_turn", "stop_sequence":
		if hasToolCalls {
			return StopToolUse, nil
		}
		return StopEndTurn, nil
	case "tool_use":
		return StopToolUse, nil
	case "max_tokens":
		return StopMaxTokens, nil
	default:
		return "", fmt.Errorf("unsupported stop reason %q", reason)
	}
}
