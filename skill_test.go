package ovr

import (
	"errors"
	"testing"
)

func TestSkillOptionAcceptsDirectoryName(t *testing.T) {
	err := Validate(
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Skill("ticket-triage"),
		),
		Reply(JSON[toolReply]()),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestSkillOptionStoresDirectoryNameOnPipeConfig(t *testing.T) {
	node := Pipe("triage ticket",
		Model("anthropic/claude-sonnet-4-6"),
		Skill("ticket-triage"),
	)

	pipe, ok := node.(pipeNode)
	if !ok {
		t.Fatalf("Pipe returned %T, want pipeNode", node)
	}
	if got, want := len(pipe.config.skills), 1; got != want {
		t.Fatalf("skill count = %d, want %d", got, want)
	}
	if got := pipe.config.skills[0].dirName; got != "ticket-triage" {
		t.Fatalf("skill dirName = %q, want ticket-triage", got)
	}
}

func TestSkillOptionRejectsInvalidDirectoryNames(t *testing.T) {
	tests := []struct {
		name string
		opt  PipeOption
	}{
		{name: "empty", opt: Skill(" ")},
		{name: "current directory", opt: Skill(".")},
		{name: "parent directory", opt: Skill("..")},
		{name: "nested path", opt: Skill("shared/ticket-triage")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From("POST /tickets"),
				Pipe("triage ticket", Model("anthropic/claude-sonnet-4-6"), tt.opt),
				Reply(JSON[toolReply]()),
			)
			if !errors.Is(err, ErrInvalidNode) {
				t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
			}
		})
	}
}
