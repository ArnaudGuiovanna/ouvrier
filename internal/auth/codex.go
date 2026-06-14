// Package auth bridges the official Codex CLI for ChatGPT-subscription sign-in.
// It never reads or stores OAuth tokens — credentials, refresh, and billing stay
// inside Codex. We only probe status and orchestrate the device-auth flow.
package auth

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
)

// AuthState is the result of a fast, cheap probe.
type AuthState string

const (
	StateAuthed   AuthState = "authed"
	StateUnauthed AuthState = "unauthed"
	StateNoCodex  AuthState = "no_codex"
)

// Runner is the exec seam (tests substitute it).
type Runner interface {
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath(file string) (string, error)
}

type defaultRunner struct{}

func (defaultRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
func (defaultRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Codex bridges the local codex CLI.
type Codex struct {
	Runner Runner
	Bin    string
}

func (c *Codex) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return defaultRunner{}
}

func (c *Codex) bin() string {
	if strings.TrimSpace(c.Bin) != "" {
		return c.Bin
	}
	return "codex"
}

// Probe runs `codex login status` (cheap, no model call) and classifies the
// result. The account label is best-effort.
func (c *Codex) Probe(ctx context.Context) (AuthState, string) {
	r := c.runner()
	bin := c.bin()
	if _, err := r.LookPath(bin); err != nil {
		return StateNoCodex, ""
	}
	cmd := r.CommandContext(ctx, bin, "login", "status")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	text := out.String()
	low := strings.ToLower(text)
	if strings.Contains(low, "logged in") && !strings.Contains(low, "not logged in") {
		return StateAuthed, strings.TrimSpace(firstLine(text))
	}
	return StateUnauthed, ""
}

// DeviceEvent is the parsed first stage of a device-auth flow.
type DeviceEvent struct {
	URL  string
	Code string
	Raw  string
}

var (
	urlRE  = regexp.MustCompile(`https?://[^\s'"]+`)
	codeRE = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)
)

// DeviceLogin starts `codex login --device-auth`, reads its output, and
// tolerantly extracts a verification URL and one-time code. Raw always carries
// the unparsed output so the UI can degrade gracefully if the format shifts.
func (c *Codex) DeviceLogin(ctx context.Context) (DeviceEvent, error) {
	r := c.runner()
	bin := c.bin()
	if _, err := r.LookPath(bin); err != nil {
		return DeviceEvent{}, err
	}
	cmd := r.CommandContext(ctx, bin, "login", "--device-auth")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	raw := out.String()
	ev := DeviceEvent{Raw: raw}
	if m := urlRE.FindString(raw); m != "" {
		ev.URL = m
	}
	if m := codeRE.FindString(raw); m != "" {
		ev.Code = m
	}
	return ev, nil
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}
