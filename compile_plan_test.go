package ovr

import (
	"reflect"
	"testing"

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
