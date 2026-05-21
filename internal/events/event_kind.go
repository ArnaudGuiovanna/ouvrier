package events

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
	EventSignatureDecision   EventKind = "signature_decision"

	EventSchemaValidationPassed EventKind = "schema_validation_passed"
	EventSchemaValidationFailed EventKind = "schema_validation_failed"
	EventSchemaRepairStarted    EventKind = "schema_repair_started"
	EventSchemaRepairCompleted  EventKind = "schema_repair_completed"
	EventSchemaRepairFailed     EventKind = "schema_repair_failed"

	EventBudgetExceeded EventKind = "budget_exceeded"

	EventTaskStarted   EventKind = "task_started"
	EventTaskCompleted EventKind = "task_completed"
	EventTaskFailed    EventKind = "task_failed"

	EventSkillLoaded EventKind = "skill_loaded"

	EventSinkLogged EventKind = "sink_logged"
)

const (
	EventSessionStart    EventKind = EventSessionStarted
	EventSessionEnd      EventKind = EventSessionSaved
	EventBeforeLLM       EventKind = EventLLMCallStarted
	EventAfterLLM        EventKind = EventLLMCallCompleted
	EventBeforeTool      EventKind = EventToolCallStarted
	EventAfterTool       EventKind = EventToolCallCompleted
	EventSchemaViolation EventKind = EventSchemaValidationFailed
	EventSubAgentStop    EventKind = EventTaskCompleted
)

var legacyEventKinds = map[EventKind]EventKind{
	"session_start":    EventSessionStarted,
	"session_end":      EventSessionSaved,
	"before_llm":       EventLLMCallStarted,
	"after_llm":        EventLLMCallCompleted,
	"before_tool":      EventToolCallStarted,
	"after_tool":       EventToolCallCompleted,
	"schema_violation": EventSchemaValidationFailed,
	"subagent_stop":    EventTaskCompleted,
}

func CanonicalKind(kind EventKind) EventKind {
	if canonical, ok := legacyEventKinds[kind]; ok {
		return canonical
	}
	return kind
}
