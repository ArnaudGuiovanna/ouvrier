package ovr

import (
	"context"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

type capturedSpan struct {
	name    string
	ended   bool
	errored bool
	err     error
}

type captureTracer struct {
	mu    sync.Mutex
	spans []*capturedSpan
}

func (t *captureTracer) StartSpan(ctx context.Context, name string, _ map[string]any) (context.Context, events.Span) {
	span := &capturedSpan{name: name}
	t.mu.Lock()
	t.spans = append(t.spans, span)
	t.mu.Unlock()
	return ctx, &captureSpan{parent: t, span: span}
}

type captureSpan struct {
	parent *captureTracer
	span   *capturedSpan
}

func (s *captureSpan) End() {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	s.span.ended = true
}
func (s *captureSpan) SetAttribute(_ string, _ any) {}
func (s *captureSpan) RecordError(err error) {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	s.span.errored = true
	s.span.err = err
}

func TestNewRunnerWithTracerRejectsNil(t *testing.T) {
	runner := NewRunner(WithTracer(nil))
	if runner == nil {
		t.Fatal("NewRunner returned nil runner")
	}
	if err := runner.Run(":0"); err == nil {
		t.Fatal("Run() returned nil error, want tracer-required error")
	}
}

func TestNewRunnerWithTracerExposesPublicTracerType(t *testing.T) {
	tracer := &captureTracer{}
	runner := NewRunner(WithTracer(tracer))
	if runner == nil || runner.err != nil {
		t.Fatalf("WithTracer set unexpected error: %v", runner.err)
	}
	if runner.tracer == nil {
		t.Fatal("runner.tracer is nil")
	}
	// Drive the tracer through the events package directly to prove WithTracer
	// installed it as a real Tracer (not a wrapped no-op).
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	if err := stream.Subscribe(events.TracerSubscriber(runner.tracer)); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Append(context.Background(), events.Event{Kind: events.EventLLMCallStarted, TraceID: "trace-a", Payload: map[string]any{"call_id": "c1"}}); err != nil {
		t.Fatalf("Append start: %v", err)
	}
	if _, err := stream.Append(context.Background(), events.Event{Kind: events.EventLLMCallCompleted, TraceID: "trace-a", Payload: map[string]any{"call_id": "c1"}}); err != nil {
		t.Fatalf("Append complete: %v", err)
	}
	if got := len(tracer.spans); got != 1 {
		t.Fatalf("span count = %d, want 1", got)
	}
	if !tracer.spans[0].ended {
		t.Fatalf("span not ended")
	}
}

func TestWithOTLPExporterRejectsEmptyEndpoint(t *testing.T) {
	runner := NewRunner(WithOTLPExporter(""))
	if runner == nil {
		t.Fatal("NewRunner returned nil runner")
	}
	if err := runner.Run(":0"); err == nil {
		t.Fatal("Run() returned nil error, want endpoint-required error")
	}
}

func TestWithOTLPExporterInstallsTracer(t *testing.T) {
	runner := NewRunner(WithOTLPExporter("https://collector.example.com:4318",
		OTLPServiceName("svc"),
		OTLPHeaders(map[string]string{"Authorization": "Bearer x"}),
	))
	if runner == nil || runner.err != nil {
		t.Fatalf("WithOTLPExporter set unexpected error: %v", runner.err)
	}
	if runner.tracer == nil {
		t.Fatal("runner.tracer is nil; WithOTLPExporter did not install a tracer")
	}
}

func TestNopTracerExposedAtPublicAPI(t *testing.T) {
	tracer := NopTracer()
	_, span := tracer.StartSpan(context.Background(), "x", nil)
	span.End()
}
