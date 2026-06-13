package codex

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestExecArgsUsesJSONSandboxAndSchemaFile(t *testing.T) {
	args, cleanup, err := execArgs(operate.TurnRequest{
		Sandbox:      operate.SandboxWorkspaceWrite,
		OutputSchema: `{"type":"object"}`,
	}, "do work")
	if err != nil {
		t.Fatalf("execArgs() error = %v", err)
	}
	defer cleanup()

	got := strings.Join(args, " ")
	for _, want := range []string{"exec", "--json", "--sandbox workspace-write", "--output-schema"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args = %v, missing %q", args, want)
		}
	}
	if args[len(args)-1] != "do work" {
		t.Fatalf("last arg = %q, want prompt", args[len(args)-1])
	}
	schemaPath := args[len(args)-2]
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("schema file missing: %v", err)
	}
	if string(data) != `{"type":"object"}` {
		t.Fatalf("schema file = %q", data)
	}
}

func TestNormalizeJSONLMapsFinalAndCommandEvents(t *testing.T) {
	event, text := normalizeJSONL(`{"type":"item.completed","item":{"text":"done"}}`)
	if event.Kind != operate.EventFinal || text != "done" {
		t.Fatalf("final event = %+v text=%q", event, text)
	}

	event, text = normalizeJSONL(`{"type":"item.completed","item":{"command":"go test ./..."}}`)
	if event.Kind != operate.EventCommandFinished || event.Command != "go test ./..." || text != "" {
		t.Fatalf("command event = %+v text=%q", event, text)
	}
}

func TestMapCodexErrMentionsLoginForAuthFailures(t *testing.T) {
	err := mapCodexErr(errors.New("exit status 1"), "Unauthorized: login required")
	if err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("error = %v, want codex login guidance", err)
	}
}
