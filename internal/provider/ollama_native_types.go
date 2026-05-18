package provider

import "encoding/json"

type ollamaNativeRequest struct {
	Model    string                `json:"model"`
	Messages []ollamaNativeMessage `json:"messages"`
	Tools    []ollamaNativeTool    `json:"tools,omitempty"`
	Stream   bool                  `json:"stream"`
}

type ollamaNativeMessage struct {
	Role      string                 `json:"role"`
	Content   string                 `json:"content,omitempty"`
	ToolCalls []ollamaNativeToolCall `json:"tool_calls,omitempty"`
	ToolName  string                 `json:"tool_name,omitempty"`
}

type ollamaNativeToolCall struct {
	Type     string                   `json:"type,omitempty"`
	Function ollamaNativeFunctionCall `json:"function"`
}

type ollamaNativeFunctionCall struct {
	Index     *int            `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type ollamaNativeTool struct {
	Type     string                   `json:"type"`
	Function ollamaNativeToolFunction `json:"function"`
}

type ollamaNativeToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ollamaNativeResponse struct {
	Message struct {
		Content   string                 `json:"content"`
		ToolCalls []ollamaNativeToolCall `json:"tool_calls"`
	} `json:"message"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}
