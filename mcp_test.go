package ovr

import (
	"errors"
	"testing"
)

func TestMCPOptionAcceptsServerName(t *testing.T) {
	err := Validate(
		From("POST /tickets"),
		Pipe("triage ticket",
			Model("anthropic/claude-sonnet-4-6"),
			MCP("moodle-mcp"),
		),
		Reply(JSON[toolReply]()),
	)
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestMCPOptionStoresServerNameOnPipeConfig(t *testing.T) {
	node := Pipe("triage ticket",
		Model("anthropic/claude-sonnet-4-6"),
		MCP("moodle-mcp"),
	)

	pipe, ok := node.(pipeNode)
	if !ok {
		t.Fatalf("Pipe returned %T, want pipeNode", node)
	}
	if got, want := len(pipe.config.mcpServers), 1; got != want {
		t.Fatalf("MCP server count = %d, want %d", got, want)
	}
	if got := pipe.config.mcpServers[0].name; got != "moodle-mcp" {
		t.Fatalf("MCP server name = %q, want moodle-mcp", got)
	}
}

func TestMCPOptionRejectsInvalidServerNames(t *testing.T) {
	tests := []struct {
		name string
		opt  PipeOption
	}{
		{name: "empty", opt: MCP(" ")},
		{name: "current directory", opt: MCP(".")},
		{name: "parent directory", opt: MCP("..")},
		{name: "nested path", opt: MCP("shared/moodle")},
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
