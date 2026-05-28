package provider

import "encoding/json"

type openAICompatRequest struct {
	Model         string                  `json:"model"`
	Messages      []openAICompatMessage   `json:"messages"`
	Tools         []openAICompatTool      `json:"tools,omitempty"`
	Stream        bool                    `json:"stream,omitempty"`
	StreamOptions *openAICompatStreamOpts `json:"stream_options,omitempty"`
}

type openAICompatStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAICompatStreamChunk struct {
	Choices []openAICompatStreamChoice `json:"choices"`
	Usage   *openAICompatUsage         `json:"usage"`
}

type openAICompatStreamChoice struct {
	Delta        openAICompatStreamDelta `json:"delta"`
	FinishReason string                  `json:"finish_reason"`
}

type openAICompatStreamDelta struct {
	Content   string                       `json:"content"`
	ToolCalls []openAICompatStreamToolCall `json:"tool_calls"`
}

type openAICompatStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
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
