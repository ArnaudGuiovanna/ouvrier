package events

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEventStreamAppendsEventsWithMonotonicIDs(t *testing.T) {
	now := time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC)
	stream, err := NewEventStream(WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	first, err := stream.Append(context.Background(), Event{
		Kind:      EventSessionStart,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		Payload:   map[string]any{"model": "anthropic/claude-sonnet-4-6"},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	second, err := stream.Append(context.Background(), Event{Kind: EventAfterLLM})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if first.ID != 1 || second.ID != 2 {
		t.Fatalf("IDs = %d, %d; want 1, 2", first.ID, second.ID)
	}
	if !first.At.Equal(now) {
		t.Fatalf("At = %v, want %v", first.At, now)
	}
	if first.Payload["model"] != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("Payload = %+v", first.Payload)
	}
}

func TestEventStreamReturnsDefensiveCopies(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	_, err = stream.Append(context.Background(), Event{
		Kind:    EventBeforeTool,
		Payload: map[string]any{"tool": "lookup"},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	events := stream.List()
	events[0].Kind = EventAfterTool
	events[0].Payload["tool"] = "mutated"

	again := stream.List()
	if again[0].Kind != EventBeforeTool {
		t.Fatalf("stored kind = %q, want %q", again[0].Kind, EventBeforeTool)
	}
	if again[0].Payload["tool"] != "lookup" {
		t.Fatalf("stored payload = %+v", again[0].Payload)
	}
}

func TestEventStreamAppendHonorsCanceledContext(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = stream.Append(ctx, Event{Kind: EventSchemaViolation})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Append error = %v, want context.Canceled", err)
	}
	if len(stream.List()) != 0 {
		t.Fatalf("events = %d, want 0", len(stream.List()))
	}
}

func TestEventStreamSinceFiltersByID(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	_, _ = stream.Append(context.Background(), Event{Kind: EventBeforeLLM})
	_, _ = stream.Append(context.Background(), Event{Kind: EventAfterLLM})
	_, _ = stream.Append(context.Background(), Event{Kind: EventSessionEnd})

	events := stream.Since(1)
	if len(events) != 2 {
		t.Fatalf("Since returned %d events, want 2", len(events))
	}
	if events[0].ID != 2 || events[1].ID != 3 {
		t.Fatalf("Since IDs = %d, %d; want 2, 3", events[0].ID, events[1].ID)
	}
}
