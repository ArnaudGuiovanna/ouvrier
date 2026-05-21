package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const geminiDefaultToolSchema = `{"type":"object","properties":{}}`

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func buildGeminiRequest(req Request) (geminiRequest, error) {
	contents, err := geminiContentsFromProvider(req.Messages)
	if err != nil {
		return geminiRequest{}, err
	}
	tools, err := geminiToolsFromProvider(req.Tools)
	if err != nil {
		return geminiRequest{}, err
	}
	out := geminiRequest{Contents: contents, Tools: tools}
	if strings.TrimSpace(req.System) != "" {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: req.System}}}
	}
	return out, nil
}

func geminiContentsFromProvider(messages []Message) ([]geminiContent, error) {
	out := make([]geminiContent, 0, len(messages))
	for _, message := range messages {
		converted, err := geminiContentFromProvider(message)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func geminiContentFromProvider(message Message) (geminiContent, error) {
	switch message.Role {
	case RoleUser:
		return geminiTextContent("user", message.Text()), nil
	case RoleAssistant:
		return geminiModelContent(message.Blocks)
	case RoleTool:
		return geminiToolResponseContent(message.Blocks)
	default:
		return geminiContent{}, fmt.Errorf("unsupported gemini role %q", message.Role)
	}
}

func geminiModelContent(blocks []Block) (geminiContent, error) {
	content := geminiContent{Role: "model"}
	for _, block := range blocks {
		switch block.Type {
		case BlockText:
			content.Parts = append(content.Parts, geminiPart{Text: block.Text})
		case BlockToolCall:
			if block.ToolCall == nil {
				return geminiContent{}, fmt.Errorf("gemini tool call block is nil")
			}
			content.Parts = append(content.Parts, geminiPart{
				FunctionCall: &geminiFunctionCall{
					ID:   block.ToolCall.ID,
					Name: block.ToolCall.Name,
					Args: geminiArguments(block.ToolCall.Arguments),
				},
			})
		}
	}
	return content, nil
}

func geminiToolResponseContent(blocks []Block) (geminiContent, error) {
	content := geminiContent{Role: "user"}
	for _, block := range blocks {
		if block.Type != BlockToolResult {
			continue
		}
		if block.ToolResult == nil {
			return geminiContent{}, fmt.Errorf("gemini tool result block is nil")
		}
		response, err := geminiToolResponsePayload(block.ToolResult)
		if err != nil {
			return geminiContent{}, err
		}
		content.Parts = append(content.Parts, geminiPart{
			FunctionResponse: &geminiFunctionResponse{
				ID:       block.ToolResult.ToolCallID,
				Name:     block.ToolResult.Name,
				Response: response,
			},
		})
	}
	return content, nil
}

func geminiToolsFromProvider(tools []ToolSpec) ([]geminiTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	declarations := make([]geminiFunctionDeclaration, 0, len(tools))
	for _, tool := range tools {
		declaration, err := geminiFunctionDeclarationFromProvider(tool)
		if err != nil {
			return nil, err
		}
		declarations = append(declarations, declaration)
	}
	return []geminiTool{{FunctionDeclarations: declarations}}, nil
}

func geminiFunctionDeclarationFromProvider(tool ToolSpec) (geminiFunctionDeclaration, error) {
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		return geminiFunctionDeclaration{}, errors.New("tool name is required")
	}
	schema := bytes.TrimSpace(tool.InputSchema)
	if len(schema) == 0 {
		schema = []byte(geminiDefaultToolSchema)
	}
	if !json.Valid(schema) {
		return geminiFunctionDeclaration{}, fmt.Errorf("tool %q input schema must be valid JSON", name)
	}
	return geminiFunctionDeclaration{
		Name:        name,
		Description: tool.Description,
		Parameters:  append(json.RawMessage(nil), schema...),
	}, nil
}
