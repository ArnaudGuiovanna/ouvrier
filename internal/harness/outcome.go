package harness

import (
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type Status string

const (
	StatusCompleted Status = "completed"
	StatusTruncated Status = "truncated"
	StatusSuspended Status = "suspended"
	StatusFailed    Status = "failed"
)

type Outcome struct {
	Status         Status
	Text           string
	Iterations     int
	Session        runtimecore.Session
	ToolCalls      []provider.ToolCall
	Usage          provider.Usage
	BudgetExceeded string
}
