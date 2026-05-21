package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"ouvrier/internal/policy"
	runtimeplan "ouvrier/internal/runtime"
)

type planReply struct {
	Status string `json:"status"`
}

type planOtherReply struct {
	OK bool `json:"ok"`
}

func TestCompilePlansCompilesHTTPPipeOutput(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets/{id}"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Output[planReply](),
		),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}

	plan := plans[0]
	if plan.Trigger.Kind != runtimeplan.TriggerHTTP {
		t.Fatalf("trigger kind = %q, want %q", plan.Trigger.Kind, runtimeplan.TriggerHTTP)
	}
	if plan.Trigger.Method != "POST" || plan.Trigger.Path != "/tickets/{id}" {
		t.Fatalf("trigger = %+v, want POST /tickets/{id}", plan.Trigger)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(plan.Steps))
	}

	step := plan.Steps[0]
	if step.Kind != runtimeplan.StepPipe {
		t.Fatalf("step kind = %q, want %q", step.Kind, runtimeplan.StepPipe)
	}
	if step.Goal != "triage ticket" {
		t.Fatalf("step goal = %q", step.Goal)
	}
	if step.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("step model = %q", step.Model)
	}
	if step.ResultSchema == nil {
		t.Fatal("step ResultSchema is nil")
	}
	if step.ResultSchema.Type != reflect.TypeFor[planReply]() {
		t.Fatalf("step ResultSchema type = %v, want planReply", step.ResultSchema.Type)
	}
	assertResultSchemaJSON(t, step.ResultSchema)

	if plan.Terminal.Kind != runtimeplan.TerminalReply {
		t.Fatalf("terminal kind = %q, want %q", plan.Terminal.Kind, runtimeplan.TerminalReply)
	}
	if plan.Terminal.ResultSchema == nil {
		t.Fatal("terminal ResultSchema is nil")
	}
	if plan.Terminal.ResultSchema.Type != reflect.TypeFor[planReply]() {
		t.Fatalf("terminal ResultSchema type = %v, want planReply", plan.Terminal.ResultSchema.Type)
	}
	assertResultSchemaJSON(t, plan.Terminal.ResultSchema)
}

func TestCompilePlansCompilesPipeRetryPolicy(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Retry(2, ExponentialBackoff()),
		),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	retry := plans[0].Steps[0].Retry
	if retry == nil {
		t.Fatal("step Retry is nil")
	}
	if retry.ProviderRetries != 2 {
		t.Fatalf("retry provider retries = %d, want 2", retry.ProviderRetries)
	}
	if retry.Backoff <= 0 {
		t.Fatalf("retry backoff = %s, want > 0", retry.Backoff)
	}
}

func TestCompilePlansCompilesPipeSkills(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Skill("ticket-triage"),
			Skill("reply-style"),
		),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	skills := plans[0].Steps[0].Skills
	if got, want := len(skills), 2; got != want {
		t.Fatalf("skills = %d, want %d", got, want)
	}
	if skills[0].Name != "ticket-triage" || skills[1].Name != "reply-style" {
		t.Fatalf("skills = %+v, want declared order", skills)
	}
}

func TestCompilePlansInjectsTerminalReplySchemaIntoFinalPipe(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	stepSchema := plans[0].Steps[0].ResultSchema
	if stepSchema == nil {
		t.Fatal("final step ResultSchema is nil")
	}
	if stepSchema.Type != reflect.TypeFor[planReply]() {
		t.Fatalf("final step ResultSchema type = %v, want planReply", stepSchema.Type)
	}
}

func TestCompilePlansRejectsMismatchedPipeOutputAndReplySchema(t *testing.T) {
	_, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Output[planReply](),
		),
		Reply(JSON[planOtherReply]()),
	})
	if !errors.Is(err, ErrIncompatibleTerminal) {
		t.Fatalf("compilePlans error = %v, want ErrIncompatibleTerminal", err)
	}
}

func TestCompatibleResultSchemasRejectsSameNameWithDifferentJSONSchema(t *testing.T) {
	left := &runtimeplan.ResultSchema{Name: "same", JSONSchema: []byte(`{"type":"object"}`)}
	right := &runtimeplan.ResultSchema{Name: "same", JSONSchema: []byte(`{"type":"string"}`)}

	if compatibleResultSchemas(left, right) {
		t.Fatal("compatibleResultSchemas returned true for same name with different JSON Schema")
	}
}

func TestCompilePlansLeavesPipeRetryPolicyUnsetByDefault(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	if retry := plans[0].Steps[0].Retry; retry != nil {
		t.Fatalf("step Retry = %+v, want nil default policy", retry)
	}
}

func TestCompilePlansPublishesStrictSingleValueToolSchema(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /learners"),
		Pipe("inspect learners",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("list_learners", listLearners,
				Param("days", "Number of days to inspect."),
			),
		),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	tool := plans[0].Steps[0].Tools[0]
	if tool.ArgumentName != "days" {
		t.Fatalf("tool ArgumentName = %q, want days", tool.ArgumentName)
	}
	var raw map[string]any
	if err := json.Unmarshal(tool.InputSchema, &raw); err != nil {
		t.Fatalf("tool InputSchema is not JSON: %v", err)
	}
	if raw["additionalProperties"] != false {
		t.Fatalf("tool InputSchema = %s, want strict additionalProperties false", tool.InputSchema)
	}
	required, ok := raw["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "days" {
		t.Fatalf("tool InputSchema required = %+v, want days", raw["required"])
	}
}

func assertResultSchemaJSON(t *testing.T, resultSchema *runtimeplan.ResultSchema) {
	t.Helper()
	if len(resultSchema.JSONSchema) == 0 {
		t.Fatal("ResultSchema JSONSchema is empty")
	}
	var raw map[string]any
	if err := json.Unmarshal(resultSchema.JSONSchema, &raw); err != nil {
		t.Fatalf("ResultSchema JSONSchema is not JSON: %v", err)
	}
	if raw["type"] != "object" {
		t.Fatalf("ResultSchema type = %v, want object", raw["type"])
	}
	if _, ok := raw["additionalProperties"]; !ok {
		t.Fatalf("ResultSchema should be strict: %s", resultSchema.JSONSchema)
	}
}

func TestCompilePlansCompilesPipeTools(t *testing.T) {
	lookup := func(ctx context.Context, args struct {
		Query string `json:"query"`
	}) (planReply, error) {
		return planReply{}, nil
	}
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup", lookup,
				Describe("Lookup ticket data."),
				Param("query", "Search query."),
				ReadOnly(),
			),
			MCP("moodle-mcp"),
		),
		Reply(JSON[planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	tools := plans[0].Steps[0].Tools
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].Name != "lookup" || tools[0].Description != "Lookup ticket data." {
		t.Fatalf("tool = %+v", tools[0])
	}
	if tools[0].GoFunc == nil {
		t.Fatal("tool GoFunc is nil")
	}
	if string(tools[0].InputSchema) == "" {
		t.Fatal("tool InputSchema is empty")
	}
	if tools[0].Effect != policy.EffectReadOnly {
		t.Fatalf("tool Effect = %q, want read_only", tools[0].Effect)
	}
	mcpServers := plans[0].Steps[0].MCPServers
	if len(mcpServers) != 1 || mcpServers[0].Name != "moodle-mcp" {
		t.Fatalf("MCP servers = %+v, want moodle-mcp", mcpServers)
	}
}

func TestCompilePlansCompilesMultiplePipelines(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("GET /health"),
		Reply(JSON[planReply]()),
		From("POST /events"),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	if plans[0].Trigger.Path != "/health" {
		t.Fatalf("first path = %q, want /health", plans[0].Trigger.Path)
	}
	if plans[0].Terminal.Kind != runtimeplan.TerminalReply {
		t.Fatalf("first terminal = %q, want reply", plans[0].Terminal.Kind)
	}
	if plans[1].Trigger.Path != "/events" {
		t.Fatalf("second path = %q, want /events", plans[1].Trigger.Path)
	}
	if plans[1].Terminal.Kind != runtimeplan.TerminalSink {
		t.Fatalf("second terminal = %q, want sink", plans[1].Terminal.Kind)
	}
}

func TestCompilePlansCompilesFileSink(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket", Model("anthropic/claude-sonnet-4-6")),
		Sink(File("./out/tickets.jsonl")),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}

	terminal := plans[0].Terminal
	if terminal.Kind != runtimeplan.TerminalSink {
		t.Fatalf("terminal kind = %q, want %q", terminal.Kind, runtimeplan.TerminalSink)
	}
	if terminal.SinkFilePath != "./out/tickets.jsonl" {
		t.Fatalf("terminal file path = %q, want ./out/tickets.jsonl", terminal.SinkFilePath)
	}
}

func TestCompilePlansCompilesLogSink(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /events"),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}

	terminal := plans[0].Terminal
	if terminal.Kind != runtimeplan.TerminalSink {
		t.Fatalf("terminal kind = %q, want %q", terminal.Kind, runtimeplan.TerminalSink)
	}
	if !terminal.SinkLog {
		t.Fatal("terminal SinkLog = false, want true for Sink(Log())")
	}
}

func TestCompilePlansCompilesWebhookPush(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Webhook("https://example.com/hook")),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}

	terminal := plans[0].Terminal
	if terminal.Kind != runtimeplan.TerminalPush {
		t.Fatalf("terminal kind = %q, want %q", terminal.Kind, runtimeplan.TerminalPush)
	}
	if terminal.PushWebhookURL != "https://example.com/hook" {
		t.Fatalf("terminal webhook URL = %q, want https://example.com/hook", terminal.PushWebhookURL)
	}
}

func TestCompilePlansCompilesQueuePush(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("triage ticket", Model("anthropic/claude-sonnet-4-6")),
		Push(Queue("nats://127.0.0.1:4222/tickets.classified")),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(plans))
	}

	terminal := plans[0].Terminal
	if terminal.Kind != runtimeplan.TerminalPush {
		t.Fatalf("terminal kind = %q, want %q", terminal.Kind, runtimeplan.TerminalPush)
	}
	if terminal.PushQueueURI != "nats://127.0.0.1:4222/tickets.classified" {
		t.Fatalf("terminal queue URI = %q, want nats://127.0.0.1:4222/tickets.classified", terminal.PushQueueURI)
	}
}

func TestCompilePlansCompilesAcceptedReply(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /jobs", WorkerPool(2)),
		Pipe("process job", Model("anthropic/claude-sonnet-4-6")),
		Reply(Accepted()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	terminal := plans[0].Terminal
	if terminal.Kind != runtimeplan.TerminalReply {
		t.Fatalf("terminal kind = %q, want reply", terminal.Kind)
	}
	if !terminal.Async {
		t.Fatal("terminal Async = false, want true")
	}
	if plans[0].Trigger.WorkerPool != 2 {
		t.Fatalf("worker pool = %d, want 2", plans[0].Trigger.WorkerPool)
	}
}

func TestCompilePlansCompilesSSEReply(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("GET /events"),
		Pipe("stream status", Model("anthropic/claude-sonnet-4-6")),
		Reply(SSE()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	terminal := plans[0].Terminal
	if terminal.Kind != runtimeplan.TerminalReply {
		t.Fatalf("terminal kind = %q, want reply", terminal.Kind)
	}
	if !terminal.SSE {
		t.Fatal("terminal SSE = false, want true")
	}
	if terminal.Async {
		t.Fatal("terminal Async = true, want false")
	}
}
