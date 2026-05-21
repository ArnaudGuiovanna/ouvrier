package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func buildOllamaNativeRequest(model string, req Request) (ollamaNativeRequest, error) {
	out := ollamaNativeRequest{
		Model:    model,
		Messages: make([]ollamaNativeMessage, 0, len(req.Messages)+1),
		Stream:   false,
	}
	if strings.TrimSpace(req.System) != "" {
		out.Messages = append(out.Messages, ollamaNativeMessage{Role: "system", Content: req.System})
	}
	for _, message := range req.Messages {
		converted, err := ollamaNativeMessageFromProvider(message)
		if err != nil {
			return ollamaNativeRequest{}, err
		}
		out.Messages = append(out.Messages, converted)
	}
	tools, err := ollamaNativeToolsFromProvider(req.Tools)
	if err != nil {
		return ollamaNativeRequest{}, err
	}
	out.Tools = tools
	return out, nil
}

func ollamaNativeMessageFromProvider(message Message) (ollamaNativeMessage, error) {
	role, err := ollamaNativeRole(message.Role)
	if err != nil {
		return ollamaNativeMessage{}, err
	}
	out := ollamaNativeMessage{Role: role}
	var content []string
	for _, block := range message.Blocks {
		if err := addOllamaNativeBlock(&out, &content, block); err != nil {
			return ollamaNativeMessage{}, err
		}
	}
	out.Content = strings.Join(content, "\n")
	return out, nil
}

func addOllamaNativeBlock(out *ollamaNativeMessage, content *[]string, block Block) error {
	switch block.Type {
	case BlockText:
		*content = append(*content, block.Text)
	case BlockToolCall:
		if block.ToolCall == nil {
			return fmt.Errorf("assistant tool call block is nil")
		}
		index := len(out.ToolCalls)
		out.ToolCalls = append(out.ToolCalls, ollamaNativeToolCall{
			Type: "function",
			Function: ollamaNativeFunctionCall{
				Index:     &index,
				Name:      block.ToolCall.Name,
				Arguments: ollamaNativeArguments(block.ToolCall.Arguments),
			},
		})
	case BlockToolResult:
		if block.ToolResult == nil {
			return fmt.Errorf("tool result block is nil")
		}
		out.ToolName = block.ToolResult.Name
		*content = append(*content, ollamaNativeToolContent(block.ToolResult.Content))
	default:
		return fmt.Errorf("unsupported ollama block type %q", block.Type)
	}
	return nil
}

func ollamaNativeRole(role Role) (string, error) {
	switch role {
	case RoleUser:
		return "user", nil
	case RoleAssistant:
		return "assistant", nil
	case RoleTool:
		return "tool", nil
	default:
		return "", fmt.Errorf("unsupported ollama role %q", role)
	}
}

func ollamaNativeToolsFromProvider(specs []ToolSpec) ([]ollamaNativeTool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	tools := make([]ollamaNativeTool, 0, len(specs))
	for _, spec := range specs {
		tool, err := ollamaNativeToolFromProvider(spec)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func ollamaNativeToolFromProvider(spec ToolSpec) (ollamaNativeTool, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return ollamaNativeTool{}, errors.New("tool name is required")
	}
	schema := bytes.TrimSpace(spec.InputSchema)
	if len(schema) == 0 {
		schema = []byte(`{"type":"object","properties":{}}`)
	}
	if !json.Valid(schema) {
		return ollamaNativeTool{}, fmt.Errorf("tool %q input schema must be valid JSON", name)
	}
	return ollamaNativeTool{
		Type: "function",
		Function: ollamaNativeToolFunction{
			Name:        name,
			Description: spec.Description,
			Parameters:  append(json.RawMessage(nil), schema...),
		},
	}, nil
}
