package provider

import (
	"encoding/json"
	"fmt"
)

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResponse struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func buildAnthropicRequest(model string, req Request) (anthropicRequest, error) {
	messages, err := anthropicMessagesFromProvider(req.Messages)
	if err != nil {
		return anthropicRequest{}, err
	}
	return anthropicRequest{
		Model:     model,
		MaxTokens: defaultAnthropicMaxTokens,
		System:    req.System,
		Messages:  messages,
		Tools:     anthropicToolsFromProvider(req.Tools),
	}, nil
}

func anthropicMessagesFromProvider(messages []Message) ([]anthropicMessage, error) {
	out := make([]anthropicMessage, 0, len(messages))
	for _, message := range messages {
		converted, err := anthropicMessageFromProvider(message)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func anthropicMessageFromProvider(message Message) (anthropicMessage, error) {
	role := string(message.Role)
	if message.Role == RoleTool {
		role = string(RoleUser)
	}
	blocks, err := anthropicBlocksFromProvider(message.Blocks)
	if err != nil {
		return anthropicMessage{}, err
	}
	return anthropicMessage{Role: role, Content: blocks}, nil
}

func anthropicBlocksFromProvider(blocks []Block) ([]anthropicBlock, error) {
	out := make([]anthropicBlock, 0, len(blocks))
	for _, block := range blocks {
		converted, err := anthropicBlockFromProvider(block)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

func anthropicBlockFromProvider(block Block) (anthropicBlock, error) {
	switch block.Type {
	case BlockText:
		return anthropicBlock{Type: "text", Text: block.Text}, nil
	case BlockToolCall:
		if block.ToolCall == nil {
			return anthropicBlock{}, fmt.Errorf("anthropic tool call block is nil")
		}
		return anthropicBlock{
			Type:  "tool_use",
			ID:    block.ToolCall.ID,
			Name:  block.ToolCall.Name,
			Input: block.ToolCall.Arguments,
		}, nil
	case BlockToolResult:
		if block.ToolResult == nil {
			return anthropicBlock{}, fmt.Errorf("anthropic tool result block is nil")
		}
		return anthropicBlock{
			Type:      "tool_result",
			ToolUseID: block.ToolResult.ToolCallID,
			Content:   string(block.ToolResult.Content),
			IsError:   block.ToolResult.IsError,
		}, nil
	default:
		return anthropicBlock{}, fmt.Errorf("unsupported anthropic block type %q", block.Type)
	}
}

func anthropicToolsFromProvider(tools []ToolSpec) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return out
}

func (r anthropicResponse) toProviderResponse() (Response, error) {
	resp := Response{
		StopReason: anthropicStopReason(r.StopReason),
		Usage: Usage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
		},
	}
	for _, block := range r.Content {
		switch block.Type {
		case "text":
			resp.Text += block.Text
		case "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: block.Input,
			})
		}
	}
	return resp, nil
}

func anthropicStopReason(reason string) StopReason {
	switch reason {
	case "end_turn":
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	default:
		return StopReason(reason)
	}
}
