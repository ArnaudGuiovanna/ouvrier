package events

import (
	"context"
	"errors"
	"testing"
)

func TestHookBusRunsHooksInOrder(t *testing.T) {
	bus := NewHookBus()
	var calls []string
	if err := bus.Register(EventBeforeLLM, func(ctx context.Context, event Event) (Event, error) {
		calls = append(calls, "first")
		event.Payload["first"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := bus.Register(EventBeforeLLM, func(ctx context.Context, event Event) (Event, error) {
		calls = append(calls, "second")
		if event.Payload["first"] != true {
			t.Fatalf("second hook did not see first hook payload: %+v", event.Payload)
		}
		event.Payload["second"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	event, err := bus.Emit(context.Background(), Event{
		Kind:    EventBeforeLLM,
		Payload: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("calls = %+v, want first then second", calls)
	}
	if event.Payload["first"] != true || event.Payload["second"] != true {
		t.Fatalf("event payload = %+v, want hook enrichments", event.Payload)
	}
}

func TestHookBusReturnsBlockingHookError(t *testing.T) {
	bus := NewHookBus()
	boom := errors.New("blocked")
	if err := bus.Register(EventBeforeTool, func(ctx context.Context, event Event) (Event, error) {
		return event, boom
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	_, err := bus.Emit(context.Background(), Event{Kind: EventBeforeTool})
	if !errors.Is(err, boom) {
		t.Fatalf("Emit error = %v, want blocking hook error", err)
	}
}

func TestHookBusRejectsNilHook(t *testing.T) {
	bus := NewHookBus()
	if err := bus.Register(EventAfterLLM, nil); err == nil {
		t.Fatal("Register returned nil for nil hook")
	}
}

func TestHookBusRunsLegacyHookAliasForCanonicalEvent(t *testing.T) {
	bus := NewHookBus()
	called := false
	if err := bus.Register(EventKind("before_llm"), func(ctx context.Context, event Event) (Event, error) {
		called = true
		if event.Kind != EventLLMCallStarted {
			t.Fatalf("hook event kind = %q, want %q", event.Kind, EventLLMCallStarted)
		}
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	event, err := bus.Emit(context.Background(), Event{Kind: EventLLMCallStarted})
	if err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if !called {
		t.Fatal("legacy before_llm hook was not called for canonical LLM event")
	}
	if event.Kind != EventLLMCallStarted {
		t.Fatalf("emitted kind = %q, want %q", event.Kind, EventLLMCallStarted)
	}
}

func TestHookBusRunsCanonicalHookForLegacyEvent(t *testing.T) {
	bus := NewHookBus()
	called := false
	if err := bus.Register(EventToolCallStarted, func(ctx context.Context, event Event) (Event, error) {
		called = true
		if event.Kind != EventToolCallStarted {
			t.Fatalf("hook event kind = %q, want %q", event.Kind, EventToolCallStarted)
		}
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	event, err := bus.Emit(context.Background(), Event{Kind: EventKind("before_tool")})
	if err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if !called {
		t.Fatal("canonical tool hook was not called for legacy before_tool event")
	}
	if event.Kind != EventToolCallStarted {
		t.Fatalf("emitted kind = %q, want %q", event.Kind, EventToolCallStarted)
	}
}
