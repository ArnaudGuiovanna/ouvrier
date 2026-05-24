package state

import (
	"context"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type Store interface {
	SaveExecution(context.Context, Execution) error
	Execution(context.Context, string) (Execution, bool, error)
	Executions(context.Context) ([]Execution, error)
	SaveSession(context.Context, runtimecore.Session) error
	Session(context.Context, string) (runtimecore.Session, bool, error)
	Sessions(context.Context) ([]runtimecore.Session, error)
	ReserveIdempotency(context.Context, string, string) (string, bool, error)
	AddEvent(context.Context, events.Event) (events.Event, error)
	Events(context.Context, string) ([]events.Event, error)
	EventsSince(context.Context, string, uint64) ([]events.Event, error)
	AddSchemaViolation(context.Context, SchemaViolation) (SchemaViolation, error)
	SchemaViolations(context.Context, string) ([]SchemaViolation, error)
}

type ExecutionStatus string

const (
	ExecutionRunning   ExecutionStatus = "running"
	ExecutionCompleted ExecutionStatus = "completed"
	ExecutionFailed    ExecutionStatus = "failed"
	ExecutionTruncated ExecutionStatus = "truncated"
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
