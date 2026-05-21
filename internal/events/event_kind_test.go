package events

import "testing"

func TestEventKindSpecMinimumConstants(t *testing.T) {
	tests := map[string]EventKind{
		"pipeline_started":         EventPipelineStarted,
		"pipeline_completed":       EventPipelineCompleted,
		"pipeline_failed":          EventPipelineFailed,
		"pipe_started":             EventPipeStarted,
		"pipe_completed":           EventPipeCompleted,
		"pipe_failed":              EventPipeFailed,
		"session_started":          EventSessionStarted,
		"session_saved":            EventSessionSaved,
		"session_cancelled":        EventSessionCancelled,
		"llm_call_started":         EventLLMCallStarted,
		"llm_call_completed":       EventLLMCallCompleted,
		"llm_call_failed":          EventLLMCallFailed,
		"tool_call_started":        EventToolCallStarted,
		"tool_call_completed":      EventToolCallCompleted,
		"tool_call_failed":         EventToolCallFailed,
		"permission_decision":      EventPermissionDecision,
		"schema_validation_passed": EventSchemaValidationPassed,
		"schema_validation_failed": EventSchemaValidationFailed,
		"schema_repair_started":    EventSchemaRepairStarted,
		"schema_repair_completed":  EventSchemaRepairCompleted,
		"schema_repair_failed":     EventSchemaRepairFailed,
		"budget_exceeded":          EventBudgetExceeded,
		"task_started":             EventTaskStarted,
		"task_completed":           EventTaskCompleted,
		"task_failed":              EventTaskFailed,
	}

	for want, got := range tests {
		if got != EventKind(want) {
			t.Fatalf("%s constant = %q, want %q", want, got, want)
		}
	}
}

func TestCanonicalKindMapsLegacyEventNames(t *testing.T) {
	tests := map[EventKind]EventKind{
		EventKind("session_start"):    EventSessionStarted,
		EventKind("session_end"):      EventSessionSaved,
		EventKind("before_llm"):       EventLLMCallStarted,
		EventKind("after_llm"):        EventLLMCallCompleted,
		EventKind("before_tool"):      EventToolCallStarted,
		EventKind("after_tool"):       EventToolCallCompleted,
		EventKind("schema_violation"): EventSchemaValidationFailed,
		EventKind("subagent_stop"):    EventTaskCompleted,
		EventKind("custom_event"):     EventKind("custom_event"),
	}

	for legacy, want := range tests {
		if got := CanonicalKind(legacy); got != want {
			t.Fatalf("CanonicalKind(%q) = %q, want %q", legacy, got, want)
		}
	}
}

func TestEventStreamStoresCanonicalKindForLegacyEvent(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	event, err := stream.Append(t.Context(), Event{Kind: EventKind("before_llm")})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if event.Kind != EventLLMCallStarted {
		t.Fatalf("appended kind = %q, want %q", event.Kind, EventLLMCallStarted)
	}
	events := stream.List()
	if events[0].Kind != EventLLMCallStarted {
		t.Fatalf("stored kind = %q, want %q", events[0].Kind, EventLLMCallStarted)
	}
}
