// Package auth bridges the official Codex CLI for ChatGPT-subscription sign-in.
// It never reads or stores OAuth tokens — credentials, refresh, and billing stay
// inside Codex. We only probe status and orchestrate the device-auth flow.
package auth

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AuthState is the result of a fast, cheap probe.
type AuthState string

const (
	StateAuthed   AuthState = "authed"
	StateUnauthed AuthState = "unauthed"
	StateNoCodex  AuthState = "no_codex"
	StateNoClaude AuthState = "no_claude"
)

const (
	maxCodexAuthOutputBytes = 64 << 10
	codexAuthWaitDelay      = 250 * time.Millisecond
)

// ErrCodexAuthOutputLimit means the authentication subprocess produced more
// output than can safely be retained. DeviceLogin returns the bounded prefix
// in DeviceEvent.Raw alongside this error.
var ErrCodexAuthOutputLimit = errors.New("codex authentication output exceeds safe capture limit")

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
	if ctx == nil {
		return StateUnauthed, ""
	}
	r := c.runner()
	bin := c.bin()
	if _, err := r.LookPath(bin); err != nil {
		return StateNoCodex, ""
	}
	cmd := r.CommandContext(ctx, bin, "login", "status")
	if cmd == nil {
		return StateUnauthed, ""
	}
	out := newBoundedAuthCapture(maxCodexAuthOutputBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = codexAuthWaitDelay
	runErr := cmd.Run()
	text := out.String()
	if runErr != nil || ctx.Err() != nil || out.Truncated() {
		return StateUnauthed, ""
	}
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
	if ctx == nil {
		return DeviceEvent{}, errors.New("codex device login context is required")
	}
	r := c.runner()
	bin := c.bin()
	if _, err := r.LookPath(bin); err != nil {
		return DeviceEvent{}, err
	}
	cmd := r.CommandContext(ctx, bin, "login", "--device-auth")
	if cmd == nil {
		return DeviceEvent{}, errors.New("codex device login command is nil")
	}
	out := newBoundedAuthCapture(maxCodexAuthOutputBytes)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = codexAuthWaitDelay
	runErr := cmd.Run()
	raw := out.String()
	ev := DeviceEvent{Raw: raw}
	if m := urlRE.FindString(raw); m != "" {
		ev.URL = m
	}
	if m := codeRE.FindString(raw); m != "" {
		ev.Code = m
	}
	if err := ctx.Err(); err != nil {
		return ev, err
	}
	if out.Truncated() {
		return ev, fmt.Errorf("%w (%d bytes)", ErrCodexAuthOutputLimit, maxCodexAuthOutputBytes)
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return ev, runErr
	}
	return ev, nil
}

type boundedAuthCapture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func newBoundedAuthCapture(limit int) *boundedAuthCapture {
	if limit < 0 {
		limit = 0
	}
	return &boundedAuthCapture{remaining: limit}
}

func (b *boundedAuthCapture) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(data)
	keep := len(data)
	if keep > b.remaining {
		keep = b.remaining
		b.truncated = true
	}
	if keep > 0 {
		_, _ = b.buf.Write(data[:keep])
		b.remaining -= keep
	}
	return written, nil
}

func (b *boundedAuthCapture) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *boundedAuthCapture) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}
