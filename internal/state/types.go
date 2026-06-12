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
	SaveMemory(context.Context, string, string, string) error
	Memory(context.Context, string, string) (string, bool, error)
	ListMemory(context.Context, string) ([]MemoryRecord, error)
	SaveApproval(context.Context, PendingApproval) error
	Approval(context.Context, string) (PendingApproval, bool, error)
	PendingApprovals(context.Context) ([]PendingApproval, error)
	ResolveApproval(context.Context, string, ApprovalStatus, string) (PendingApproval, error)

	// Durable runs (step-checkpoint journal, tool intents, retention).
	// SaveRunJournal and SaveRunCheckpoint upsert on their primary keys
	// ((exec_id) and (exec_id, step_index)); BeginToolIntent upserts on
	// (exec_id, tool_call_id) and re-opens the intent (completed_at NULL).
	// CompleteToolIntent stamps completed_at and errors when no intent row
	// exists. RunCheckpoints orders by step index, ToolIntents by started_at
	// then tool call id, RunJournals by created_at then exec id.
	// PruneRunJournal deletes one execution's journal, checkpoints, and
	// intents; PruneRunJournalsBefore deletes every journal created before
	// the cutoff (cascading the same way) and returns the pruned exec ids.
	SaveRunJournal(context.Context, RunJournal) error
	RunJournal(context.Context, string) (RunJournal, bool, error)
	RunJournals(context.Context) ([]RunJournal, error)
	SaveRunCheckpoint(context.Context, RunCheckpoint) error
	RunCheckpoints(context.Context, string) ([]RunCheckpoint, error)
	BeginToolIntent(context.Context, ToolIntent) error
	CompleteToolIntent(context.Context, string, string) error
	ToolIntents(context.Context, string) ([]ToolIntent, error)
	PruneRunJournal(context.Context, string) error
	PruneRunJournalsBefore(context.Context, time.Time) ([]string, error)
}

// ApprovalStatus is the lifecycle state of a human-in-the-loop approval.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
)

// PendingApproval is one human-in-the-loop approval request that suspends an
// execution until an operator approves or denies the gated tool call. The
// Reason field is redaction-safe (run through the same credential scrubbing as
// persisted events) and never carries tool arguments or skill bodies.
type PendingApproval struct {
	ID         string
	ExecID     string
	SessionID  string
	TraceID    string
	ToolName   string
	ToolCallID string
	ToolKind   string
	Effect     string
	Reason     string
	Status     ApprovalStatus
	CreatedAt  time.Time
	DecidedAt  time.Time
	DecidedBy  string
}

// MaxMemoryValueBytes bounds the size of a single persisted memory value.
//
// Retention note: memory entries are durable across sessions and are never
// auto-expired by the store. They are keyed by (scope, key); writing the same
// (scope, key) overwrites the previous value (last-write-wins). Callers that
// need eviction should scope keys deliberately (e.g. include a generation or
// timestamp in the key) and prune via SaveMemory overwrites; a future TTL or
// LRU policy can layer on top without changing this contract.
const MaxMemoryValueBytes = 64 * 1024

// MemoryRecord is one scoped, persistent agent-memory entry.
type MemoryRecord struct {
	Scope     string
	Key       string
	Value     string
	UpdatedAt time.Time
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
