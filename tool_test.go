package ovr

import (
	"context"
	"errors"
	"testing"

	"ouvrier/internal/policy"
)

type toolReply struct {
	Status string `json:"status"`
}

type learner struct {
	ID string `json:"id"`
}

func listLearners(ctx context.Context, days int) ([]learner, error) {
	return nil, nil
}

func auditLearner(ctx context.Context, id string) error {
	return nil
}

func TestToolOptionAcceptsSupportedSignatures(t *testing.T) {
	err := Validate(
		From("POST /learners"),
		Pipe("inspect learners",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("list_learners", listLearners,
				Describe("List learners changed in the last N days."),
				Param("days", "Number of days to inspect."),
			),
			Tool("audit_learner", auditLearner),
		),
		Reply(JSON[toolReply]()),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestToolOptionStoresMetadataOnPipeConfig(t *testing.T) {
	node := Pipe("inspect learners",
		Model("anthropic/claude-sonnet-4-6"),
		Tool("list_learners", listLearners,
			Describe("List learners changed in the last N days."),
			Param("days", "Number of days to inspect."),
		),
	)

	pipe, ok := node.(pipeNode)
	if !ok {
		t.Fatalf("Pipe returned %T, want pipeNode", node)
	}
	if got, want := len(pipe.config.tools), 1; got != want {
		t.Fatalf("tool count = %d, want %d", got, want)
	}

	tool := pipe.config.tools[0]
	if tool.name != "list_learners" {
		t.Fatalf("tool name = %q, want list_learners", tool.name)
	}
	if tool.description != "List learners changed in the last N days." {
		t.Fatalf("tool description = %q", tool.description)
	}
	if got := tool.params["days"]; got != "Number of days to inspect." {
		t.Fatalf("days param description = %q", got)
	}
	if tool.fn == nil || tool.fnType == nil {
		t.Fatalf("tool function metadata was not stored")
	}
}

func TestToolOptionStoresExecutionClassification(t *testing.T) {
	node := Pipe("inspect learners",
		Model("anthropic/claude-sonnet-4-6"),
		Tool("list_learners", listLearners, ReadOnly()),
		Tool("audit_learner", auditLearner,
			Idempotent("learner.id"),
			RequiresApproval(),
		),
		Tool("email_learner", auditLearner, SideEffecting("email")),
	)

	pipe, ok := node.(pipeNode)
	if !ok {
		t.Fatalf("Pipe returned %T, want pipeNode", node)
	}
	if got, want := len(pipe.config.tools), 3; got != want {
		t.Fatalf("tool count = %d, want %d", got, want)
	}
	if pipe.config.tools[0].effect != policy.EffectReadOnly {
		t.Fatalf("first effect = %q, want read_only", pipe.config.tools[0].effect)
	}
	second := pipe.config.tools[1]
	if second.effect != policy.EffectIdempotent {
		t.Fatalf("second effect = %q, want idempotent", second.effect)
	}
	if second.idempotencyKey != "learner.id" {
		t.Fatalf("idempotency key = %q, want learner.id", second.idempotencyKey)
	}
	if !second.requiresApproval {
		t.Fatal("requiresApproval = false, want true")
	}
	third := pipe.config.tools[2]
	if third.effect != policy.EffectSideEffecting {
		t.Fatalf("third effect = %q, want side_effecting", third.effect)
	}
	if len(third.sideEffects) != 1 || third.sideEffects[0] != "email" {
		t.Fatalf("side effects = %+v, want email", third.sideEffects)
	}
}

func TestToolOptionRejectsInvalidToolDeclarations(t *testing.T) {
	var nilTool func(context.Context) error

	tests := []struct {
		name string
		opt  PipeOption
	}{
		{name: "empty name", opt: Tool(" ", listLearners)},
		{name: "nil function", opt: Tool("nil_tool", nilTool)},
		{name: "non function", opt: Tool("not_func", 42)},
		{name: "missing context", opt: Tool("missing_context", func(days int) error { return nil })},
		{name: "no returns", opt: Tool("no_returns", func(context.Context) {})},
		{name: "return without error", opt: Tool("bad_return", func(context.Context) string { return "" })},
		{name: "too many returns", opt: Tool("too_many", func(context.Context) (string, string, error) { return "", "", nil })},
		{name: "empty describe", opt: Tool("empty_describe", listLearners, Describe(" "))},
		{name: "empty param name", opt: Tool("empty_param", listLearners, Param(" ", "description"))},
		{name: "empty param description", opt: Tool("empty_param_description", listLearners, Param("days", " "))},
		{name: "empty idempotency key", opt: Tool("empty_idempotency", auditLearner, Idempotent(" "))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From("POST /learners"),
				Pipe("inspect learners", Model("anthropic/claude-sonnet-4-6"), tt.opt),
				Reply(JSON[toolReply]()),
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}
