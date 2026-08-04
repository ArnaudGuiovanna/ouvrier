package auth

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type claudeFakeRunner struct {
	output  string
	failed  bool
	missing bool
	block   bool
}

func (r claudeFakeRunner) LookPath(string) (string, error) {
	if r.missing {
		return "", exec.ErrNotFound
	}
	return "/usr/bin/claude", nil
}

func (r claudeFakeRunner) CommandContext(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	if r.block {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}
	command := "printf '%s' " + shellQuote(r.output)
	if r.failed {
		command += "; exit 9"
	}
	return exec.CommandContext(ctx, "sh", "-c", command)
}

func TestClaudeProbeAuthed(t *testing.T) {
	state, method := (&Claude{Runner: claudeFakeRunner{output: `{"loggedIn":true,"authMethod":"claude.ai"}`}}).Probe(context.Background())
	if state != StateAuthed || method != "claude.ai" {
		t.Fatalf("Probe() = %q, %q", state, method)
	}
}

func TestClaudeProbeFailsClosed(t *testing.T) {
	tests := []claudeFakeRunner{
		{output: `{"loggedIn":false}`},
		{output: `{"loggedIn":true}`, failed: true},
		{output: strings.Repeat("x", maxClaudeAuthOutputBytes+1)},
	}
	for _, runner := range tests {
		state, method := (&Claude{Runner: runner}).Probe(context.Background())
		if state != StateUnauthed || method != "" {
			t.Fatalf("Probe() = %q, %q; want fail-closed unauthenticated", state, method)
		}
	}
}

func TestClaudeProbeReportsMissingCLI(t *testing.T) {
	state, _ := (&Claude{Runner: claudeFakeRunner{missing: true}}).Probe(context.Background())
	if state != StateNoClaude {
		t.Fatalf("Probe() = %q, want %q", state, StateNoClaude)
	}
}

func TestClaudeProbeHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	state, _ := (&Claude{Runner: claudeFakeRunner{block: true}}).Probe(ctx)
	if state != StateUnauthed {
		t.Fatalf("Probe() = %q", state)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("probe exceeded deadline: %s", elapsed)
	}
}
