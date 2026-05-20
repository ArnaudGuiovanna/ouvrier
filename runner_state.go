package ovr

import (
	"context"
	"time"

	internalevents "ouvrier/internal/events"
	runtimecore "ouvrier/internal/runtime"
	internalstate "ouvrier/internal/state"
)

// Budget captures execution limits recorded with public sessions.
type Budget struct {
	MaxIterations int
	MaxTokens     int
	MaxCostUSD    float64
	MaxWallClock  time.Duration
}

// Session is the public state-store representation of one runtime session.
type Session struct {
	ExecID          string
	SessionID       string
	ParentSessionID string
	TraceID         string
	Model           string
	StartedAt       time.Time
	Budget          Budget
}

// ExecutionStatus is the public state-store status for one execution.
type ExecutionStatus string

const (
	ExecutionRunning   ExecutionStatus = "running"
	ExecutionCompleted ExecutionStatus = "completed"
	ExecutionFailed    ExecutionStatus = "failed"
	ExecutionTruncated ExecutionStatus = "truncated"
)

// Execution is the public state-store representation of one pipeline execution.
type Execution struct {
	ExecID      string
	TraceID     string
	Status      ExecutionStatus
	StartedAt   time.Time
	CompletedAt time.Time
}

// SchemaViolation is the public state-store representation of one schema failure.
type SchemaViolation struct {
	ID         uint64
	At         time.Time
	ExecID     string
	SessionID  string
	SchemaName string
	Error      string
}

// StateStore is the public durable execution store contract accepted by NewRunner.
type StateStore interface {
	SaveExecution(context.Context, Execution) error
	Execution(context.Context, string) (Execution, bool, error)
	Executions(context.Context) ([]Execution, error)
	SaveSession(context.Context, Session) error
	Session(context.Context, string) (Session, bool, error)
	Sessions(context.Context) ([]Session, error)
	ReserveIdempotency(context.Context, string, string) (string, bool, error)
	AddEvent(context.Context, Event) (Event, error)
	Events(context.Context, string) ([]Event, error)
	EventsSince(context.Context, string, uint64) ([]Event, error)
	AddSchemaViolation(context.Context, SchemaViolation) (SchemaViolation, error)
	SchemaViolations(context.Context, string) ([]SchemaViolation, error)
}

type publicStateStoreAdapter struct {
	store StateStore
}

func (s publicStateStoreAdapter) SaveExecution(ctx context.Context, execution internalstate.Execution) error {
	return s.store.SaveExecution(ctx, publicExecution(execution))
}

func (s publicStateStoreAdapter) Execution(ctx context.Context, execID string) (internalstate.Execution, bool, error) {
	execution, ok, err := s.store.Execution(ctx, execID)
	return internalExecution(execution), ok, err
}

func (s publicStateStoreAdapter) Executions(ctx context.Context) ([]internalstate.Execution, error) {
	executions, err := s.store.Executions(ctx)
	if err != nil {
		return nil, err
	}
	internal := make([]internalstate.Execution, len(executions))
	for i, execution := range executions {
		internal[i] = internalExecution(execution)
	}
	return internal, nil
}

func (s publicStateStoreAdapter) SaveSession(ctx context.Context, session runtimecore.Session) error {
	return s.store.SaveSession(ctx, publicSession(session))
}

func (s publicStateStoreAdapter) Session(ctx context.Context, sessionID string) (runtimecore.Session, bool, error) {
	session, ok, err := s.store.Session(ctx, sessionID)
	return internalSession(session), ok, err
}

func (s publicStateStoreAdapter) Sessions(ctx context.Context) ([]runtimecore.Session, error) {
	sessions, err := s.store.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	internal := make([]runtimecore.Session, len(sessions))
	for i, session := range sessions {
		internal[i] = internalSession(session)
	}
	return internal, nil
}

func (s publicStateStoreAdapter) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
	return s.store.ReserveIdempotency(ctx, key, execID)
}

func (s publicStateStoreAdapter) AddEvent(ctx context.Context, event internalevents.Event) (internalevents.Event, error) {
	added, err := s.store.AddEvent(ctx, publicEvent(event))
	return internalEvent(added), err
}

func (s publicStateStoreAdapter) Events(ctx context.Context, execID string) ([]internalevents.Event, error) {
	events, err := s.store.Events(ctx, execID)
	if err != nil {
		return nil, err
	}
	internal := make([]internalevents.Event, len(events))
	for i, event := range events {
		internal[i] = internalEvent(event)
	}
	return internal, nil
}

func (s publicStateStoreAdapter) EventsSince(ctx context.Context, execID string, afterID uint64) ([]internalevents.Event, error) {
	events, err := s.store.EventsSince(ctx, execID, afterID)
	if err != nil {
		return nil, err
	}
	internal := make([]internalevents.Event, len(events))
	for i, event := range events {
		internal[i] = internalEvent(event)
	}
	return internal, nil
}

func (s publicStateStoreAdapter) AddSchemaViolation(ctx context.Context, violation internalstate.SchemaViolation) (internalstate.SchemaViolation, error) {
	added, err := s.store.AddSchemaViolation(ctx, publicSchemaViolation(violation))
	return internalSchemaViolation(added), err
}

func (s publicStateStoreAdapter) SchemaViolations(ctx context.Context, execID string) ([]internalstate.SchemaViolation, error) {
	violations, err := s.store.SchemaViolations(ctx, execID)
	if err != nil {
		return nil, err
	}
	internal := make([]internalstate.SchemaViolation, len(violations))
	for i, violation := range violations {
		internal[i] = internalSchemaViolation(violation)
	}
	return internal, nil
}

func publicExecution(execution internalstate.Execution) Execution {
	return Execution{
		ExecID:      execution.ExecID,
		TraceID:     execution.TraceID,
		Status:      ExecutionStatus(execution.Status),
		StartedAt:   execution.StartedAt,
		CompletedAt: execution.CompletedAt,
	}
}

func internalExecution(execution Execution) internalstate.Execution {
	return internalstate.Execution{
		ExecID:      execution.ExecID,
		TraceID:     execution.TraceID,
		Status:      internalstate.ExecutionStatus(execution.Status),
		StartedAt:   execution.StartedAt,
		CompletedAt: execution.CompletedAt,
	}
}

func publicSession(session runtimecore.Session) Session {
	return Session{
		ExecID:          session.ExecID,
		SessionID:       session.SessionID,
		ParentSessionID: session.ParentSessionID,
		TraceID:         session.TraceID,
		Model:           session.Model,
		StartedAt:       session.StartedAt,
		Budget:          publicBudget(session.Budget),
	}
}

func internalSession(session Session) runtimecore.Session {
	return runtimecore.Session{
		ExecID:          session.ExecID,
		SessionID:       session.SessionID,
		ParentSessionID: session.ParentSessionID,
		TraceID:         session.TraceID,
		Model:           session.Model,
		StartedAt:       session.StartedAt,
		Budget:          internalBudget(session.Budget),
	}
}

func publicBudget(budget runtimecore.Budget) Budget {
	return Budget{
		MaxIterations: budget.MaxIterations,
		MaxTokens:     budget.MaxTokens,
		MaxCostUSD:    budget.MaxCostUSD,
		MaxWallClock:  budget.MaxWallClock,
	}
}

func internalBudget(budget Budget) runtimecore.Budget {
	return runtimecore.Budget{
		MaxIterations: budget.MaxIterations,
		MaxTokens:     budget.MaxTokens,
		MaxCostUSD:    budget.MaxCostUSD,
		MaxWallClock:  budget.MaxWallClock,
	}
}

func publicSchemaViolation(violation internalstate.SchemaViolation) SchemaViolation {
	return SchemaViolation{
		ID:         violation.ID,
		At:         violation.At,
		ExecID:     violation.ExecID,
		SessionID:  violation.SessionID,
		SchemaName: violation.SchemaName,
		Error:      violation.Error,
	}
}

func internalSchemaViolation(violation SchemaViolation) internalstate.SchemaViolation {
	return internalstate.SchemaViolation{
		ID:         violation.ID,
		At:         violation.At,
		ExecID:     violation.ExecID,
		SessionID:  violation.SessionID,
		SchemaName: violation.SchemaName,
		Error:      violation.Error,
	}
}
