package harness

import (
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
)

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
	Session    runtimecore.Session
	ToolCalls  []provider.ToolCall
	Usage      provider.Usage
}
