package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolCall   BlockType = "tool_call"
	BlockToolResult BlockType = "tool_result"
)

type Message struct {
	Role   Role
	Blocks []Block
}

func UserText(text string) Message {
	return Message{Role: RoleUser, Blocks: []Block{TextBlock(text)}}
}

func AssistantText(text string) Message {
	return Message{Role: RoleAssistant, Blocks: []Block{TextBlock(text)}}
}

func AssistantToolCalls(text string, calls ...ToolCall) Message {
	blocks := make([]Block, 0, len(calls)+1)
	if text != "" {
		blocks = append(blocks, TextBlock(text))
	}
	for _, call := range calls {
		call := call
		blocks = append(blocks, Block{Type: BlockToolCall, ToolCall: &call})
	}
	return Message{Role: RoleAssistant, Blocks: blocks}
}

func ToolResultText(call ToolCall, text string, isError bool) Message {
	content, _ := json.Marshal(text)
	return Message{
		Role: RoleTool,
		Blocks: []Block{{
			Type: BlockToolResult,
			ToolResult: &ToolResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    content,
				IsError:    isError,
			},
		}},
	}
}

func TextBlock(text string) Block {
	return Block{Type: BlockText, Text: text}
}

func (m Message) Text() string {
	var text []string
	for _, block := range m.Blocks {
		if block.Type == BlockText && block.Text != "" {
			text = append(text, block.Text)
		}
	}
	return strings.Join(text, "\n")
}

func (m Message) Validate() error {
	switch m.Role {
	case RoleUser, RoleAssistant, RoleTool:
	default:
		return fmt.Errorf("unknown message role %q", m.Role)
	}
	if len(m.Blocks) == 0 {
		return errors.New("message must contain at least one block")
	}
	for _, block := range m.Blocks {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type Block struct {
	Type       BlockType
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
}

func (b Block) Validate() error {
	switch b.Type {
	case BlockText:
		if b.Text == "" {
			return errors.New("text block must not be empty")
		}
	case BlockToolCall:
		if b.ToolCall == nil {
			return errors.New("tool call block requires a tool call")
		}
		return b.ToolCall.Validate()
	case BlockToolResult:
		if b.ToolResult == nil {
			return errors.New("tool result block requires a tool result")
		}
		return b.ToolResult.Validate()
	default:
		return fmt.Errorf("unknown block type %q", b.Type)
	}
	return nil
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

func (c ToolCall) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("tool call ID is required")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("tool call name is required")
	}
	return nil
}

type ToolResult struct {
	ToolCallID string
	Name       string
	Content    json.RawMessage
	IsError    bool
}

func (r ToolResult) Validate() error {
	if strings.TrimSpace(r.ToolCallID) == "" {
		return errors.New("tool result call ID is required")
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("tool result name is required")
	}
	return nil
}
