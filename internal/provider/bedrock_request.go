package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func buildBedrockConverseRequest(req Request) (bedrockConverseRequest, error) {
	out := bedrockConverseRequest{}
	if system := strings.TrimSpace(req.System); system != "" {
		out.System = []bedrockSystemBlock{{Text: req.System}}
	}

	for _, msg := range req.Messages {
		converted, err := bedrockMessageFromProvider(msg)
		if err != nil {
			return bedrockConverseRequest{}, err
		}
		out.Messages = append(out.Messages, converted)
	}
	if len(out.Messages) == 0 {
		return bedrockConverseRequest{}, errors.New("request must include at least one message")
	}

	tools, err := buildBedrockTools(req.Tools)
	if err != nil {
		return bedrockConverseRequest{}, err
	}
	if len(tools) > 0 {
		out.ToolConfig = &bedrockToolConfig{Tools: tools}
	}
	return out, nil
}

func bedrockMessageFromProvider(msg Message) (bedrockMessage, error) {
	switch msg.Role {
	case RoleUser:
		return bedrockTextMessage("user", msg)
	case RoleAssistant:
		return bedrockAssistantMessage(msg)
	case RoleTool:
		// Bedrock represents tool results as user-role messages.
		return bedrockToolResultMessage(msg)
	default:
		return bedrockMessage{}, fmt.Errorf("unsupported message role %q", msg.Role)
	}
}

func bedrockTextMessage(role string, msg Message) (bedrockMessage, error) {
	content := make([]bedrockContentBlock, 0, len(msg.Blocks))
	for _, block := range msg.Blocks {
		if block.Type != BlockText {
			return bedrockMessage{}, fmt.Errorf("%s message cannot contain %s block", role, block.Type)
		}
		content = append(content, bedrockContentBlock{Text: block.Text})
	}
	return bedrockMessage{Role: role, Content: content}, nil
}

func bedrockAssistantMessage(msg Message) (bedrockMessage, error) {
	content := make([]bedrockContentBlock, 0, len(msg.Blocks))
	for _, block := range msg.Blocks {
		switch block.Type {
		case BlockText:
			content = append(content, bedrockContentBlock{Text: block.Text})
		case BlockToolCall:
			call := *block.ToolCall
			if err := call.Validate(); err != nil {
				return bedrockMessage{}, err
			}
			input := bytes.TrimSpace(call.Arguments)
			if len(input) == 0 {
				input = []byte(`{}`)
			}
			if !json.Valid(input) {
				return bedrockMessage{}, fmt.Errorf("tool call %q arguments must be valid JSON", call.ID)
			}
			content = append(content, bedrockContentBlock{ToolUse: &bedrockToolUse{
				ToolUseID: call.ID,
				Name:      call.Name,
				Input:     append(json.RawMessage(nil), input...),
			}})
		default:
			return bedrockMessage{}, fmt.Errorf("assistant message cannot contain %s block", block.Type)
		}
	}
	return bedrockMessage{Role: "assistant", Content: content}, nil
}

func bedrockToolResultMessage(msg Message) (bedrockMessage, error) {
	content := make([]bedrockContentBlock, 0, len(msg.Blocks))
	for _, block := range msg.Blocks {
		if block.Type != BlockToolResult {
			return bedrockMessage{}, fmt.Errorf("tool message cannot contain %s block", block.Type)
		}
		result := block.ToolResult
		resultContent, err := bedrockToolResultBlocks(result.Content)
		if err != nil {
			return bedrockMessage{}, err
		}
		tr := &bedrockToolResult{
			ToolUseID: result.ToolCallID,
			Content:   resultContent,
		}
		if result.IsError {
			tr.Status = "error"
		}
		content = append(content, bedrockContentBlock{ToolResult: tr})
	}
	return bedrockMessage{Role: "user", Content: content}, nil
}

func bedrockToolResultBlocks(raw json.RawMessage) ([]bedrockToolResultContent, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []bedrockToolResultContent{{Text: ""}}, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []bedrockToolResultContent{{Text: text}}, nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("tool result content must be valid JSON")
	}
	return []bedrockToolResultContent{{JSON: append(json.RawMessage(nil), raw...)}}, nil
}

func buildBedrockTools(specs []ToolSpec) ([]bedrockTool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]bedrockTool, 0, len(specs))
	for _, spec := range specs {
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			return nil, errors.New("tool name is required")
		}
		schema := bytes.TrimSpace(spec.InputSchema)
		if len(schema) == 0 {
			schema = []byte(defaultOpenAICompatToolSchema)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("tool %q input schema must be valid JSON", name)
		}
		tools = append(tools, bedrockTool{ToolSpec: bedrockToolSpec{
			Name:        name,
			Description: spec.Description,
			InputSchema: bedrockToolInputSchema{JSON: append(json.RawMessage(nil), schema...)},
		}})
	}
	return tools, nil
}
