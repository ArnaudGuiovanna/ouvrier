package provider

import (
	"context"
	"encoding/json"
	"time"
)

// Provider is the LLM boundary used by the agent harness.
type Provider interface {
	Name() string
	Complete(context.Context, Request) (Response, error)
}

type Request struct {
	Model    string
	System   string
	Messages []Message
	Tools    []ToolSpec
	CacheKey string
}

func (r Request) Validate() error {
	if _, err := ParseModelID(r.Model); err != nil {
		return err
	}
	for _, msg := range r.Messages {
		if err := msg.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type Response struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason StopReason
	Usage      Usage
	Metadata   ResponseMetadata
}

type ResponseMetadata struct {
	Provider    string
	Model       string
	Latency     time.Duration
	PromptCache PromptCacheMetadata
}

type PromptCacheMetadata struct {
	Requested        bool
	Supported        bool
	Applied          bool
	CacheKey         string
	ReadInputTokens  int
	WriteInputTokens int
	Reason           string
}

type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
)

type Usage struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
}

func (u *Usage) Add(next Usage) {
	u.InputTokens += next.InputTokens
	u.OutputTokens += next.OutputTokens
	u.CostUSD += next.CostUSD
}

type ToolSpec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}
