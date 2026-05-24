package events

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestNopTracerEndDoesNotPanic(t *testing.T) {
	_, span := NopTracer().StartSpan(context.Background(), "test", nil)
	span.SetAttribute("k", "v")
	span.RecordError(errors.New("ignored"))
	span.End()
}

type recordedSpan struct {
	name    string
	attrs   map[string]any
	errored bool
	err     error
	ended   bool
}

type recordingTracer struct {
	mu    sync.Mutex
	spans []*recordedSpan
}

func (r *recordingTracer) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span) {
	span := &recordedSpan{name: name, attrs: cloneAttrs(attrs)}
	r.mu.Lock()
	r.spans = append(r.spans, span)
	r.mu.Unlock()
	return ctx, &recordingSpan{span: span, parent: r}
}

func cloneAttrs(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type recordingSpan struct {
	span   *recordedSpan
	parent *recordingTracer
}

func (s *recordingSpan) End() {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	s.span.ended = true
}

func (s *recordingSpan) SetAttribute(key string, value any) {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	if s.span.attrs == nil {
		s.span.attrs = map[string]any{}
	}
	s.span.attrs[key] = value
}

func (s *recordingSpan) RecordError(err error) {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	s.span.errored = true
	s.span.err = err
}

func TestTracerSubscriberPairsStartAndComplete(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	tracer := &recordingTracer{}
	if err := stream.Subscribe(TracerSubscriber(tracer)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx := context.Background()
	if _, err := stream.Append(ctx, Event{Kind: EventPipelineStarted, ExecID: "exec-1", TraceID: "trace-1"}); err != nil {
		t.Fatalf("Append start: %v", err)
	}
	if _, err := stream.Append(ctx, Event{Kind: EventPipelineCompleted, ExecID: "exec-1", TraceID: "trace-1", Payload: map[string]any{"duration_ms": 42}}); err != nil {
		t.Fatalf("Append complete: %v", err)
	}

	if got := len(tracer.spans); got != 1 {
		t.Fatalf("span count = %d, want 1", got)
	}
	span := tracer.spans[0]
	if span.name != "pipeline" {
		t.Fatalf("span name = %q, want pipeline", span.name)
	}
	if !span.ended {
		t.Fatalf("span not ended")
	}
	if span.errored {
		t.Fatalf("span recorded error unexpectedly")
	}
	if span.attrs["duration_ms"] != 42 {
		t.Fatalf("attrs = %v, want duration_ms=42", span.attrs)
	}
}

func TestTracerSubscriberRecordsFailure(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	tracer := &recordingTracer{}
	if err := stream.Subscribe(TracerSubscriber(tracer)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx := context.Background()
	if _, err := stream.Append(ctx, Event{Kind: EventToolCallStarted, TraceID: "trace-2", Payload: map[string]any{"call_id": "call-x", "name": "lookup"}}); err != nil {
		t.Fatalf("Append start: %v", err)
	}
	if _, err := stream.Append(ctx, Event{Kind: EventToolCallFailed, TraceID: "trace-2", Payload: map[string]any{"call_id": "call-x", "error": "permission denied"}}); err != nil {
		t.Fatalf("Append fail: %v", err)
	}

	if got := len(tracer.spans); got != 1 {
		t.Fatalf("span count = %d, want 1", got)
	}
	span := tracer.spans[0]
	if !span.errored {
		t.Fatalf("expected span to record error")
	}
	if span.err == nil || span.err.Error() != "permission denied" {
		t.Fatalf("recorded error = %v, want \"permission denied\"", span.err)
	}
	if !span.ended {
		t.Fatalf("span not ended on failure")
	}
}

func TestTracerSubscriberOrphanCompleteIsIgnored(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	tracer := &recordingTracer{}
	if err := stream.Subscribe(TracerSubscriber(tracer)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Append(context.Background(), Event{Kind: EventPipelineCompleted, TraceID: "trace-3"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := len(tracer.spans); got != 0 {
		t.Fatalf("span count = %d, want 0", got)
	}
}

func TestSubscriberPanicDoesNotBreakStream(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	if err := stream.Subscribe(func(_ context.Context, _ Event) { panic("boom") }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Append(context.Background(), Event{Kind: EventPipelineStarted}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := len(stream.List()); got != 1 {
		t.Fatalf("event count = %d, want 1", got)
	}
}

func TestEventStreamSubscribeRejectsNil(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	if err := stream.Subscribe(nil); err == nil {
		t.Fatalf("Subscribe(nil) returned nil error")
	}
}
