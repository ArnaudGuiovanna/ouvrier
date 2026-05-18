package provider

import "encoding/json"

type openAICompatRequest struct {
	Model    string                `json:"model"`
	Messages []openAICompatMessage `json:"messages"`
	Tools    []openAICompatTool    `json:"tools,omitempty"`
}

type openAICompatMessage struct {
	Role       string                 `json:"role"`
	Content    *string                `json:"content,omitempty"`
	ToolCalls  []openAICompatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string                 `json:"tool_call_id,omitempty"`
}

type openAICompatTool struct {
	Type     string                  `json:"type"`
	Function openAICompatFunctionDef `json:"function"`
}

type openAICompatToolCall struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"`
	Function openAICompatFunctionDef `json:"function"`
}

type openAICompatFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type openAICompatResponse struct {
	Choices []openAICompatChoice `json:"choices"`
	Usage   openAICompatUsage    `json:"usage"`
}

type openAICompatChoice struct {
	Message      openAICompatResponseMessage `json:"message"`
	FinishReason string                      `json:"finish_reason"`
}

type openAICompatResponseMessage struct {
	Content   json.RawMessage        `json:"content"`
	ToolCalls []openAICompatToolCall `json:"tool_calls"`
}

type openAICompatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
