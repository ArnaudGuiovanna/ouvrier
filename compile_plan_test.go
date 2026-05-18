package ovr

import (
	"context"
	"reflect"
	"testing"

	"ouvrier/internal/policy"
	runtimeplan "ouvrier/internal/runtime"
)

type planReply struct {
	Status string `json:"status"`
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

	if plan.Terminal.Kind != runtimeplan.TerminalReply {
		t.Fatalf("terminal kind = %q, want %q", plan.Terminal.Kind, runtimeplan.TerminalReply)
	}
	if plan.Terminal.ResultSchema == nil {
		t.Fatal("terminal ResultSchema is nil")
	}
	if plan.Terminal.ResultSchema.Type != reflect.TypeFor[planReply]() {
		t.Fatalf("terminal ResultSchema type = %v, want planReply", plan.Terminal.ResultSchema.Type)
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

func TestCompilePlansCompilesAcceptedReply(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /jobs"),
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
}
