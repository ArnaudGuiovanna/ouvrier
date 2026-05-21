package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const defaultOpenAICompatToolSchema = `{"type":"object","properties":{}}`

func buildOpenAICompatRequest(model string, req Request) (openAICompatRequest, error) {
	messages, err := buildOpenAICompatMessages(req)
	if err != nil {
		return openAICompatRequest{}, err
	}
	if len(messages) == 0 {
		return openAICompatRequest{}, errors.New("request must include at least one message")
	}

	tools, err := buildOpenAICompatTools(req.Tools)
	if err != nil {
		return openAICompatRequest{}, err
	}
	return openAICompatRequest{Model: model, Messages: messages, Tools: tools}, nil
}

func buildOpenAICompatMessages(req Request) ([]openAICompatMessage, error) {
	var out []openAICompatMessage
	if strings.TrimSpace(req.System) != "" {
		system := req.System
		out = append(out, openAICompatMessage{Role: "system", Content: &system})
	}

	for _, msg := range req.Messages {
		converted, err := openAICompatMessageFromProvider(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, converted...)
	}
	return out, nil
}

func openAICompatMessageFromProvider(msg Message) ([]openAICompatMessage, error) {
	switch msg.Role {
	case RoleUser:
		converted, err := openAICompatTextMessage("user", msg)
		return []openAICompatMessage{converted}, err
	case RoleAssistant:
		converted, err := openAICompatAssistantMessage(msg)
		return []openAICompatMessage{converted}, err
	case RoleTool:
		return openAICompatToolMessages(msg)
	default:
		return nil, fmt.Errorf("unsupported message role %q", msg.Role)
	}
}

func openAICompatTextMessage(role string, msg Message) (openAICompatMessage, error) {
	var parts []string
	for _, block := range msg.Blocks {
		if block.Type != BlockText {
			return openAICompatMessage{}, fmt.Errorf("%s message cannot contain %s block", role, block.Type)
		}
		parts = append(parts, block.Text)
	}
	content := strings.Join(parts, "\n")
	return openAICompatMessage{Role: role, Content: &content}, nil
}

func openAICompatAssistantMessage(msg Message) (openAICompatMessage, error) {
	var parts []string
	var calls []openAICompatToolCall
	for _, block := range msg.Blocks {
		switch block.Type {
		case BlockText:
			parts = append(parts, block.Text)
		case BlockToolCall:
			call, err := openAICompatToolCallFromProvider(*block.ToolCall)
			if err != nil {
				return openAICompatMessage{}, err
			}
			calls = append(calls, call)
		default:
			return openAICompatMessage{}, fmt.Errorf("assistant message cannot contain %s block", block.Type)
		}
	}

	converted := openAICompatMessage{Role: "assistant", ToolCalls: calls}
	if len(parts) > 0 {
		content := strings.Join(parts, "\n")
		converted.Content = &content
	}
	return converted, nil
}

func openAICompatToolMessages(msg Message) ([]openAICompatMessage, error) {
	out := make([]openAICompatMessage, 0, len(msg.Blocks))
	for _, block := range msg.Blocks {
		if block.Type != BlockToolResult {
			return nil, fmt.Errorf("tool message cannot contain %s block", block.Type)
		}
		content, err := openAICompatToolContent(block.ToolResult.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, openAICompatMessage{
			Role:       "tool",
			Content:    &content,
			ToolCallID: block.ToolResult.ToolCallID,
		})
	}
	return out, nil
}

func openAICompatToolCallFromProvider(call ToolCall) (openAICompatToolCall, error) {
	if err := call.Validate(); err != nil {
		return openAICompatToolCall{}, err
	}
	args := bytes.TrimSpace(call.Arguments)
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	if !json.Valid(args) {
		return openAICompatToolCall{}, fmt.Errorf("tool call %q arguments must be valid JSON", call.ID)
	}
	return openAICompatToolCall{
		ID:   call.ID,
		Type: "function",
		Function: openAICompatFunctionDef{
			Name:      call.Name,
			Arguments: string(args),
		},
	}, nil
}

func openAICompatToolContent(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	if !json.Valid(raw) {
		return "", errors.New("tool result content must be valid JSON")
	}
	return string(raw), nil
}

func buildOpenAICompatTools(specs []ToolSpec) ([]openAICompatTool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]openAICompatTool, 0, len(specs))
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
		tools = append(tools, openAICompatTool{
			Type: "function",
			Function: openAICompatFunctionDef{
				Name:        name,
				Description: spec.Description,
				Parameters:  append(json.RawMessage(nil), schema...),
			},
		})
	}
	return tools, nil
}
