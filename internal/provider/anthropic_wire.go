package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`
	System    any                `json:"system,omitempty"`
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
	Cache     *anthropicCache `json:"cache_control,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Cache       *anthropicCache `json:"cache_control,omitempty"`
}

type anthropicCache struct {
	Type string `json:"type"`
}

type anthropicResponse struct {
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens   int `json:"input_tokens"`
		OutputTokens  int `json:"output_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
		CacheCreate   int `json:"cache_creation_input_tokens"`
		CacheCreation struct {
			Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
}

func buildAnthropicRequest(model string, req Request) (anthropicRequest, error) {
	messages, err := anthropicMessagesFromProvider(req.Messages)
	if err != nil {
		return anthropicRequest{}, err
	}
	system, tools, _ := anthropicPromptPrefix(req)
	return anthropicRequest{
		Model:     model,
		MaxTokens: defaultAnthropicMaxTokens,
		System:    system,
		Messages:  messages,
		Tools:     tools,
	}, nil
}

func anthropicPromptPrefix(req Request) (any, []anthropicTool, bool) {
	tools := anthropicToolsFromProvider(req.Tools)
	var system any
	if strings.TrimSpace(req.System) != "" {
		system = req.System
	}
	if strings.TrimSpace(req.CacheKey) == "" {
		return system, tools, false
	}
	cache := &anthropicCache{Type: "ephemeral"}
	if system != nil {
		system = []anthropicBlock{{
			Type:  "text",
			Text:  req.System,
			Cache: cache,
		}}
		return system, tools, true
	}
	if len(tools) > 0 {
		tools[len(tools)-1].Cache = cache
		return system, tools, true
	}
	return system, tools, false
}

func anthropicPromptCacheRequestedPrefix(req Request) bool {
	_, _, applied := anthropicPromptPrefix(req)
	return applied
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
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
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
		Metadata: ResponseMetadata{
			PromptCache: PromptCacheMetadata{
				Applied:          r.promptCacheApplied(),
				ReadInputTokens:  r.Usage.CacheRead,
				WriteInputTokens: r.promptCacheWriteTokens(),
			},
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

func (r anthropicResponse) promptCacheApplied() bool {
	return r.Usage.CacheRead > 0 || r.promptCacheWriteTokens() > 0
}

func (r anthropicResponse) promptCacheWriteTokens() int {
	return r.Usage.CacheCreate +
		r.Usage.CacheCreation.Ephemeral5m +
		r.Usage.CacheCreation.Ephemeral1h
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
