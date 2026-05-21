package ovr

import (
	"context"
	"errors"
	"sync"
	"time"

	internalevents "ouvrier/internal/events"
)

// EventKind identifies a runner lifecycle event that hooks can observe.
type EventKind string

const (
	EventPipelineStarted   EventKind = "pipeline_started"
	EventPipelineCompleted EventKind = "pipeline_completed"
	EventPipelineFailed    EventKind = "pipeline_failed"

	EventPipeStarted   EventKind = "pipe_started"
	EventPipeCompleted EventKind = "pipe_completed"
	EventPipeFailed    EventKind = "pipe_failed"

	EventSessionStarted   EventKind = "session_started"
	EventSessionSaved     EventKind = "session_saved"
	EventSessionCancelled EventKind = "session_cancelled"

	EventLLMCallStarted   EventKind = "llm_call_started"
	EventLLMCallCompleted EventKind = "llm_call_completed"
	EventLLMCallFailed    EventKind = "llm_call_failed"

	EventToolCallStarted   EventKind = "tool_call_started"
	EventToolCallCompleted EventKind = "tool_call_completed"
	EventToolCallFailed    EventKind = "tool_call_failed"

	EventPermissionDecision  EventKind = "permission_decision"
	EventIdempotencyDecision EventKind = "idempotency_decision"

	EventSchemaValidationPassed EventKind = "schema_validation_passed"
	EventSchemaValidationFailed EventKind = "schema_validation_failed"
	EventSchemaRepairStarted    EventKind = "schema_repair_started"
	EventSchemaRepairCompleted  EventKind = "schema_repair_completed"
	EventSchemaRepairFailed     EventKind = "schema_repair_failed"

	EventBudgetExceeded EventKind = "budget_exceeded"

	EventTaskStarted   EventKind = "task_started"
	EventTaskCompleted EventKind = "task_completed"
	EventTaskFailed    EventKind = "task_failed"

	EventSinkLogged EventKind = "sink_logged"
)

// Event is the public, redacted event shape passed through advanced hooks.
type Event struct {
	ID        uint64
	At        time.Time
	Kind      EventKind
	ExecID    string
	SessionID string
	TraceID   string
	Payload   map[string]any
}

// Hook observes or enriches a lifecycle event. Returning an error blocks that event.
type Hook func(context.Context, Event) (Event, error)

// Hooks is a public hook registration set consumed by NewRunner.
type Hooks struct {
	mu            sync.RWMutex
	registrations []hookRegistration
	err           error
}

type hookRegistration struct {
	kind EventKind
	hook Hook
}

// NewHooks creates an empty hook registration set.
func NewHooks() *Hooks {
	return &Hooks{}
}

// Register adds a hook for one lifecycle event kind.
func (h *Hooks) Register(kind EventKind, hook Hook) error {
	if h == nil {
		return errors.New("hooks are required")
	}
	if kind == "" {
		return errors.New("hook event kind is required")
	}
	if hook == nil {
		return errors.New("hook is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registrations = append(h.registrations, hookRegistration{kind: kind, hook: hook})
	return nil
}

func (h *Hooks) hookBus() (*internalevents.HookBus, error) {
	if h == nil {
		return nil, errors.New("hooks are required")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.err != nil {
		return nil, h.err
	}
	bus := internalevents.NewHookBus()
	for _, registration := range h.registrations {
		registration := registration
		if err := bus.Register(internalevents.EventKind(registration.kind), func(ctx context.Context, event internalevents.Event) (internalevents.Event, error) {
			next, err := registration.hook(ctx, publicEvent(event))
			if err != nil {
				return internalevents.Event{}, err
			}
			return internalEvent(next), nil
		}); err != nil {
			return nil, err
		}
	}
	return bus, nil
}

func publicEvent(event internalevents.Event) Event {
	return Event{
		ID:        event.ID,
		At:        event.At,
		Kind:      EventKind(event.Kind),
		ExecID:    event.ExecID,
		SessionID: event.SessionID,
		TraceID:   event.TraceID,
		Payload:   clonePayload(event.Payload),
	}
}

func internalEvent(event Event) internalevents.Event {
	return internalevents.Event{
		ID:        event.ID,
		At:        event.At,
		Kind:      internalevents.EventKind(event.Kind),
		ExecID:    event.ExecID,
		SessionID: event.SessionID,
		TraceID:   event.TraceID,
		Payload:   clonePayload(event.Payload),
	}
}

func clonePayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	clone := make(map[string]any, len(payload))
	for key, value := range payload {
		clone[key] = value
	}
	return clone
}
