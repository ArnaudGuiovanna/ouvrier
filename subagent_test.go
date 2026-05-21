package ovr

import (
	"errors"
	"reflect"
	"testing"

	runtimeplan "ouvrier/internal/runtime"
)

type subAgentTranslation struct {
	Text string `json:"text"`
}

type subAgentReply struct {
	Body string `json:"body"`
}

func TestCompilePlansCompilesPipeSubAgent(t *testing.T) {
	translator := Pipeline(
		Pipe("translate text",
			Model("anthropic/claude-haiku-4-5"),
			Output[subAgentTranslation](),
		),
	)

	plans, err := compilePlans([]Node{
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("translate", translator, MaxParallel(3)),
		),
		Reply(JSON[subAgentReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	step := plans[0].Steps[0]
	if len(step.SubAgents) != 1 {
		t.Fatalf("subagents = %d, want 1", len(step.SubAgents))
	}

	subAgent := step.SubAgents[0]
	if subAgent.Name != "translate" {
		t.Fatalf("subagent name = %q, want translate", subAgent.Name)
	}
	if subAgent.MaxParallel != 3 {
		t.Fatalf("subagent MaxParallel = %d, want 3", subAgent.MaxParallel)
	}
	if len(subAgent.Pipeline.Steps) != 1 {
		t.Fatalf("subagent pipeline steps = %d, want 1", len(subAgent.Pipeline.Steps))
	}

	childStep := subAgent.Pipeline.Steps[0]
	if childStep.Kind != runtimeplan.StepPipe {
		t.Fatalf("child step kind = %q, want %q", childStep.Kind, runtimeplan.StepPipe)
	}
	if childStep.Goal != "translate text" {
		t.Fatalf("child step goal = %q, want translate text", childStep.Goal)
	}
	if childStep.Model != "anthropic/claude-haiku-4-5" {
		t.Fatalf("child step model = %q", childStep.Model)
	}
	if childStep.ResultSchema == nil {
		t.Fatal("child step ResultSchema is nil")
	}
	if childStep.ResultSchema.Type != reflect.TypeFor[subAgentTranslation]() {
		t.Fatalf("child step ResultSchema type = %v, want subAgentTranslation", childStep.ResultSchema.Type)
	}
}

func TestValidateAcceptsPipeWithSubAgentPipeline(t *testing.T) {
	translator := Pipeline(
		Pipe("translate text",
			Model("anthropic/claude-haiku-4-5"),
			Output[subAgentTranslation](),
		),
	)

	err := Validate(
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("translate", translator, MaxParallel(2)),
		),
		Reply(JSON[subAgentReply]()),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestValidateRejectsSubAgentWithoutName(t *testing.T) {
	translator := Pipeline(
		Pipe("translate text", Model("anthropic/claude-haiku-4-5")),
	)

	err := Validate(
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent(" ", translator),
		),
		Reply(JSON[subAgentReply]()),
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
	}
}

func TestValidateRejectsSubAgentEmptyPipeline(t *testing.T) {
	err := Validate(
		From("POST /emails"),
		Pipe("draft multilingual email",
			Model("anthropic/claude-sonnet-4-6"),
			SubAgent("translate", Pipeline()),
		),
		Reply(JSON[subAgentReply]()),
	)
	if !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
	}
}

func TestValidateRejectsInvalidSubAgentMaxParallel(t *testing.T) {
	translator := Pipeline(
		Pipe("translate text", Model("anthropic/claude-haiku-4-5")),
	)

	tests := []struct {
		name string
		node Node
	}{
		{
			name: "zero",
			node: Pipe("draft multilingual email",
				Model("anthropic/claude-sonnet-4-6"),
				SubAgent("translate", translator, MaxParallel(0)),
			),
		},
		{
			name: "above hard cap",
			node: Pipe("draft multilingual email",
				Model("anthropic/claude-sonnet-4-6"),
				SubAgent("translate", translator, MaxParallel(6)),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From("POST /emails"),
				tt.node,
				Reply(JSON[subAgentReply]()),
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}
