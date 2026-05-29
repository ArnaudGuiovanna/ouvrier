package provider

import "encoding/json"

// Wire types for the Amazon Bedrock Converse API.
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html

type bedrockConverseRequest struct {
	Messages   []bedrockMessage     `json:"messages"`
	System     []bedrockSystemBlock `json:"system,omitempty"`
	ToolConfig *bedrockToolConfig   `json:"toolConfig,omitempty"`
}

type bedrockSystemBlock struct {
	Text string `json:"text"`
}

type bedrockMessage struct {
	Role    string                `json:"role"`
	Content []bedrockContentBlock `json:"content"`
}

type bedrockContentBlock struct {
	Text       string             `json:"text,omitempty"`
	ToolUse    *bedrockToolUse    `json:"toolUse,omitempty"`
	ToolResult *bedrockToolResult `json:"toolResult,omitempty"`
}

type bedrockToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type bedrockToolResult struct {
	ToolUseID string                     `json:"toolUseId"`
	Content   []bedrockToolResultContent `json:"content"`
	Status    string                     `json:"status,omitempty"`
}

type bedrockToolResultContent struct {
	Text string          `json:"text,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
}

type bedrockToolConfig struct {
	Tools []bedrockTool `json:"tools"`
}

type bedrockTool struct {
	ToolSpec bedrockToolSpec `json:"toolSpec"`
}

type bedrockToolSpec struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema bedrockToolInputSchema `json:"inputSchema"`
}

type bedrockToolInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type bedrockConverseResponse struct {
	Output     bedrockOutput `json:"output"`
	StopReason string        `json:"stopReason"`
	Usage      bedrockUsage  `json:"usage"`
}

type bedrockOutput struct {
	Message bedrockMessage `json:"message"`
}

type bedrockUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}
