package ovr

import (
	"errors"
	"testing"

	runtimeplan "ouvrier/internal/runtime"
)

func TestCompilePlansCompilesParallelStep(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("quality", Model("anthropic/quality")),
			Pipe("compliance", Model("anthropic/compliance")),
			PartialOK(),
		),
		Reply(JSON[[]planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	step := plans[0].Steps[0]
	if step.Kind != runtimeplan.StepParallel {
		t.Fatalf("step kind = %q, want %q", step.Kind, runtimeplan.StepParallel)
	}
	if !step.PartialOK {
		t.Fatal("step PartialOK = false, want true")
	}
	if got, want := len(step.Branches), 2; got != want {
		t.Fatalf("branches = %d, want %d", got, want)
	}
	if step.Branches[0].Steps[0].Goal != "quality" || step.Branches[1].Steps[0].Goal != "compliance" {
		t.Fatalf("branches = %+v, want declared order", step.Branches)
	}
}

func TestCompilePlansCompilesMapStep(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("list tickets", Model("anthropic/list")),
		Map(
			Concurrency(3),
			Pipe("classify", Model("anthropic/classify")),
			Pipe("summarize", Model("anthropic/summarize")),
			PartialOK(),
		),
		Reply(JSON[[]planReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	step := plans[0].Steps[1]
	if step.Kind != runtimeplan.StepMap {
		t.Fatalf("step kind = %q, want %q", step.Kind, runtimeplan.StepMap)
	}
	if step.Concurrency != 3 || !step.PartialOK {
		t.Fatalf("map config = concurrency %d partial %v, want 3 true", step.Concurrency, step.PartialOK)
	}
	if got, want := len(step.MapPipeline.Steps), 2; got != want {
		t.Fatalf("map steps = %d, want %d", got, want)
	}
}

func TestValidateRejectsInvalidParallelAndMapDeclarations(t *testing.T) {
	tests := []struct {
		name  string
		nodes []Node
	}{
		{
			name: "parallel empty",
			nodes: []Node{
				From("POST /tickets"),
				Parallel(),
				Reply(JSON[planReply]()),
			},
		},
		{
			name: "parallel non pipe",
			nodes: []Node{
				From("POST /tickets"),
				Parallel(Reply(JSON[planReply]())),
				Reply(JSON[planReply]()),
			},
		},
		{
			name: "map empty",
			nodes: []Node{
				From("POST /tickets"),
				Map(Concurrency(1)),
				Reply(JSON[planReply]()),
			},
		},
		{
			name: "map invalid concurrency",
			nodes: []Node{
				From("POST /tickets"),
				Map(Concurrency(0), Pipe("classify", Model("anthropic/classify"))),
				Reply(JSON[planReply]()),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.nodes...); !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}
