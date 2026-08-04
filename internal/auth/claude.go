package auth

import (
	"context"
	"encoding/json"
	"strings"
)

const maxClaudeAuthOutputBytes = 64 << 10

// Claude probes the official Claude Code CLI without reading or storing its
// credentials. Authentication remains entirely owned by Claude Code.
type Claude struct {
	Runner Runner
	Bin    string
}

func (c *Claude) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return defaultRunner{}
}

func (c *Claude) bin() string {
	if strings.TrimSpace(c.Bin) != "" {
		return strings.TrimSpace(c.Bin)
	}
	return "claude"
}

// Probe runs `claude auth status --json` with bounded output. The returned
// label is the non-secret auth method, never an account identifier.
func (c *Claude) Probe(ctx context.Context) (AuthState, string) {
	if ctx == nil {
		return StateUnauthed, ""
	}
	runner := c.runner()
	bin := c.bin()
	if _, err := runner.LookPath(bin); err != nil {
		return StateNoClaude, ""
	}
	cmd := runner.CommandContext(ctx, bin, "auth", "status", "--json")
	if cmd == nil {
		return StateUnauthed, ""
	}
	out := newBoundedAuthCapture(maxClaudeAuthOutputBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = codexAuthWaitDelay
	if err := cmd.Run(); err != nil || ctx.Err() != nil || out.Truncated() {
		return StateUnauthed, ""
	}
	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err := json.Unmarshal([]byte(out.String()), &status); err != nil || !status.LoggedIn {
		return StateUnauthed, ""
	}
	return StateAuthed, strings.TrimSpace(status.AuthMethod)
}
