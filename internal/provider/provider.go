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

// Delta is an incremental chunk of model output emitted while a streaming
// completion is in flight. Text carries provider token text; it is model
// output and must be treated as redaction-safe content (never a secret).
type Delta struct {
	Text string
}

// StreamingProvider is an OPTIONAL capability. Providers that implement it can
// surface token deltas as they arrive. The harness detects support via a type
// assertion and falls back to Complete for providers that do not implement it.
//
// CompleteStream invokes onDelta for each incremental text chunk and returns the
// fully assembled Response, identical to what Complete would return. onDelta may
// be nil, in which case the call behaves like Complete. onDelta must return
// quickly and must not be retained beyond the call.
type StreamingProvider interface {
	Provider
	CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
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
