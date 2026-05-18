package state

import "time"

type ExecutionStatus string

const (
	ExecutionRunning   ExecutionStatus = "running"
	ExecutionCompleted ExecutionStatus = "completed"
	ExecutionFailed    ExecutionStatus = "failed"
)

type Execution struct {
	ExecID      string
	TraceID     string
	Status      ExecutionStatus
	StartedAt   time.Time
	CompletedAt time.Time
}

type SchemaViolation struct {
	ID         uint64
	At         time.Time
	ExecID     string
	SessionID  string
	SchemaName string
	Error      string
}
