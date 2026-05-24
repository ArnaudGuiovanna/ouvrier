package events

import (
	"context"
	"maps"
	"sync"
)

// Span is an in-flight tracing span owned by a Tracer.
//
// End is called exactly once when the span completes. SetAttribute records a
// key/value pair (e.g. "model", "duration_ms") that will be reported alongside
// the span. RecordError attaches an error to the span without ending it.
type Span interface {
	End()
	SetAttribute(key string, value any)
	RecordError(err error)
}

// Tracer mints Spans for harness events. Adapters such as the optional OTel
// bridge implement this interface; the no-op default is returned by NopTracer.
type Tracer interface {
	StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span)
}

// NopTracer returns a Tracer whose spans do nothing. It is the default tracer
// used by the harness when the runtime is not configured with a real tracer.
func NopTracer() Tracer { return nopTracer{} }

type nopTracer struct{}

func (nopTracer) StartSpan(ctx context.Context, _ string, _ map[string]any) (context.Context, Span) {
	return ctx, nopSpan{}
}

type nopSpan struct{}

func (nopSpan) End()                         {}
func (nopSpan) SetAttribute(_ string, _ any) {}
func (nopSpan) RecordError(_ error)          {}

// TracerSubscriber returns a Subscriber that translates EventStream events
// into Tracer spans. It pairs *_started events with their *_completed/_failed
// counterparts so each pipeline, pipe, session, LLM call, tool call, schema
// check, and subagent task is represented as one span.
//
// Pairing key uses the event's TraceID + a per-kind discriminator from the
// payload (call_id / pipe_id / session_id / exec_id / subagent_task_id). When
// no discriminator is found, the TraceID alone is used.
//
// Subscriber is safe for concurrent use. A nil Tracer falls back to NopTracer.
func TracerSubscriber(tracer Tracer) Subscriber {
	if tracer == nil {
		tracer = NopTracer()
	}
	state := newTracerState(tracer)
	return state.handle
}

type tracerState struct {
	tracer Tracer
	mu     sync.Mutex
	open   map[string]Span
}

func newTracerState(tracer Tracer) *tracerState {
	return &tracerState{tracer: tracer, open: make(map[string]Span)}
}

func (s *tracerState) handle(ctx context.Context, event Event) {
	if s == nil || s.tracer == nil {
		return
	}
	op, action := classifyEvent(event.Kind)
	if op == "" || action == "" {
		return
	}
	key := tracerSpanKey(op, event)
	if key == "" {
		return
	}
	switch action {
	case "start":
		_, span := s.tracer.StartSpan(ctx, op, eventSpanAttrs(event))
		s.set(key, span)
	case "complete":
		span := s.take(key)
		if span == nil {
			return
		}
		for k, v := range eventSpanAttrs(event) {
			span.SetAttribute(k, v)
		}
		span.End()
	case "fail":
		span := s.take(key)
		if span == nil {
			return
		}
		for k, v := range eventSpanAttrs(event) {
			span.SetAttribute(k, v)
		}
		span.RecordError(spanErrorFromEvent(event))
		span.End()
	}
}

func (s *tracerState) set(key string, span Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open[key] = span
}

func (s *tracerState) take(key string) Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	span, ok := s.open[key]
	if !ok {
		return nil
	}
	delete(s.open, key)
	return span
}

func classifyEvent(kind EventKind) (op, action string) {
	switch kind {
	case EventPipelineStarted:
		return "pipeline", "start"
	case EventPipelineCompleted:
		return "pipeline", "complete"
	case EventPipelineFailed:
		return "pipeline", "fail"
	case EventPipeStarted:
		return "pipe", "start"
	case EventPipeCompleted:
		return "pipe", "complete"
	case EventPipeFailed:
		return "pipe", "fail"
	case EventSessionStarted:
		return "session", "start"
	case EventSessionSaved:
		return "session", "complete"
	case EventSessionCancelled:
		return "session", "fail"
	case EventLLMCallStarted:
		return "llm_call", "start"
	case EventLLMCallCompleted:
		return "llm_call", "complete"
	case EventLLMCallFailed:
		return "llm_call", "fail"
	case EventToolCallStarted:
		return "tool_call", "start"
	case EventToolCallCompleted:
		return "tool_call", "complete"
	case EventToolCallFailed:
		return "tool_call", "fail"
	case EventTaskStarted:
		return "subagent_task", "start"
	case EventTaskCompleted:
		return "subagent_task", "complete"
	case EventTaskFailed:
		return "subagent_task", "fail"
	case EventSchemaValidationPassed:
		return "schema_validation", "complete"
	case EventSchemaValidationFailed:
		return "schema_validation", "fail"
	}
	return "", ""
}

func tracerSpanKey(op string, event Event) string {
	disc := tracerDiscriminator(op, event)
	switch {
	case event.TraceID != "" && disc != "":
		return op + ":" + event.TraceID + ":" + disc
	case event.TraceID != "":
		return op + ":" + event.TraceID
	case disc != "":
		return op + ":" + disc
	default:
		return ""
	}
}

func tracerDiscriminator(op string, event Event) string {
	switch op {
	case "session", "pipeline":
		if event.SessionID != "" {
			return event.SessionID
		}
		return stringPayload(event.Payload, "session_id")
	case "pipe":
		if v := stringPayload(event.Payload, "pipe_id"); v != "" {
			return v
		}
		return stringPayload(event.Payload, "step_id")
	case "llm_call":
		if v := stringPayload(event.Payload, "llm_call_id"); v != "" {
			return v
		}
		return stringPayload(event.Payload, "call_id")
	case "tool_call":
		if v := stringPayload(event.Payload, "tool_call_id"); v != "" {
			return v
		}
		return stringPayload(event.Payload, "call_id")
	case "subagent_task":
		if v := stringPayload(event.Payload, "task_id"); v != "" {
			return v
		}
		return stringPayload(event.Payload, "subagent_task_id")
	case "schema_validation":
		return stringPayload(event.Payload, "schema")
	}
	return ""
}

func eventSpanAttrs(event Event) map[string]any {
	if len(event.Payload) == 0 && event.ExecID == "" && event.SessionID == "" {
		return nil
	}
	attrs := make(map[string]any, len(event.Payload)+3)
	if event.ExecID != "" {
		attrs["exec_id"] = event.ExecID
	}
	if event.SessionID != "" {
		attrs["session_id"] = event.SessionID
	}
	if event.TraceID != "" {
		attrs["trace_id"] = event.TraceID
	}
	maps.Copy(attrs, event.Payload)
	return attrs
}

func stringPayload(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key]
	if !ok {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func spanErrorFromEvent(event Event) error {
	msg := stringPayload(event.Payload, "error")
	if msg == "" {
		msg = string(event.Kind)
	}
	return tracerError{msg: msg}
}

type tracerError struct{ msg string }

func (e tracerError) Error() string { return e.msg }
