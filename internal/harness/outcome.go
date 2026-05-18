package harness

import "ouvrier/internal/provider"

type Status string

const (
	StatusCompleted Status = "completed"
	StatusTruncated Status = "truncated"
	StatusFailed    Status = "failed"
)

type Outcome struct {
	Status     Status
	Text       string
	Iterations int
	ToolCalls  []provider.ToolCall
	Usage      provider.Usage
}
