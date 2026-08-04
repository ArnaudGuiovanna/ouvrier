// Package acp implements the governed ACP v1 boundary used by external coding
// agents in the Ouvrier cockpit.
package acp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

// Runner is the process seam used by tests.
type Runner interface {
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath(file string) (string, error)
}

type defaultRunner struct{}

func (defaultRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func (defaultRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Driver runs one coding-agent turn over ACP's newline-delimited JSON-RPC
// stdio transport. The Ouvrier harness recognizes it as an external driver and
// therefore supplies only a disposable sanitized worker stage.
type Driver struct {
	Runner Runner
	Name   string
	Bin    string
}

const (
	processWait          = 2 * time.Second
	maxStderrBytes       = 64 << 10
	maxProtocolLineBytes = 8 << 20
	maxProtocolBytes     = 16 << 20
	maxAgentTextBytes    = 8 << 20
)

var _ operate.ExternalDriver = (*Driver)(nil)

// New constructs an ACP driver for an installed adapter such as
// claude-agent-acp or codex-acp.
func New(name, bin string) *Driver {
	return &Driver{Runner: defaultRunner{}, Name: strings.TrimSpace(name), Bin: strings.TrimSpace(bin)}
}

// ExternalDriverMarker opts this process-backed driver into Ouvrier's staged
// workspace boundary.
func (*Driver) ExternalDriverMarker() {}

// Probe reports whether the configured ACP adapter is discoverable. A protocol
// handshake is intentionally deferred to RunTurn so probing cannot create an
// agent session or trigger an authentication flow.
func (d *Driver) Probe(context.Context) (operate.Capabilities, error) {
	name := d.name()
	bin := d.bin()
	if _, err := d.runner().LookPath(bin); err != nil {
		return operate.Capabilities{}, fmt.Errorf("operate %s ACP: %s not found on PATH; install it with `%s`", name, bin, installCommand(bin))
	}
	return operate.Capabilities{Name: name, Transport: "acp/v1"}, nil
}

// RunTurn starts a fresh local ACP server, negotiates protocol v1, creates one
// session rooted at req.CWD, and runs exactly one bounded prompt turn.
func (d *Driver) RunTurn(ctx context.Context, req operate.TurnRequest, sink operate.EventSink) (operate.TurnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(req.CWD) == "" {
		return operate.TurnResult{}, errors.New("operate ACP: staged working directory is required")
	}
	path, err := d.runner().LookPath(d.bin())
	if err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate %s ACP: %s not found on PATH; install it with `%s`", d.name(), d.bin(), installCommand(d.bin()))
	}

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := d.runner().CommandContext(turnCtx, path)
	if cmd == nil {
		return operate.TurnResult{}, fmt.Errorf("operate %s ACP: adapter command is nil", d.name())
	}
	cmd.Dir = req.CWD
	cmd.Env = commandEnv(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate %s ACP: open stdin: %w", d.name(), err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate %s ACP: open stdout: %w", d.name(), err)
	}
	stderr := newBoundedCapture(maxStderrBytes)
	cmd.Stderr = stderr
	if err := configureProcess(cmd); err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate %s ACP: configure process containment: %w", d.name(), err)
	}
	if err := cmd.Start(); err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate %s ACP: start: %w", d.name(), err)
	}

	client := newClient(stdin, stdout, req, sink)
	runErr := client.run(turnCtx)
	_ = stdin.Close()
	if runErr != nil {
		cancel()
	}
	waitErr, forcedStop, cleanupErr := waitForACPProcess(cmd, cancel)
	result := client.result()

	if warning := strings.TrimSpace(stderr.String()); warning != "" && sink != nil {
		safe := req.Redactor.Redact(warning)
		if stderr.Truncated() {
			safe += "\n[ACP stderr truncated]"
		}
		if sinkErr := sink.Event(operate.Event{At: time.Now().UTC(), Kind: operate.EventWarning, Message: safe}); sinkErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("persist ACP stderr event: %w", sinkErr))
		}
	}
	if forcedStop && sink != nil {
		if sinkErr := sink.Event(operate.Event{
			At: time.Now().UTC(), Kind: operate.EventWarning,
			Message: "ACP adapter did not exit after the completed turn; Ouvrier terminated its process tree",
		}); sinkErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("persist ACP cleanup event: %w", sinkErr))
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, errors.Join(ctxErr, cleanupErr)
	}
	if runErr != nil {
		if errors.Is(runErr, ErrAuthenticationRequired) {
			return result, errors.Join(fmt.Errorf("operate %s ACP: %w; the saved local agent session is unavailable", d.name(), runErr), cleanupErr)
		}
		return result, errors.Join(fmt.Errorf("operate %s ACP: %w", d.name(), runErr), cleanupErr)
	}
	if stderr.Truncated() {
		return result, errors.Join(fmt.Errorf("operate %s ACP: stderr exceeds %d bytes", d.name(), maxStderrBytes), cleanupErr)
	}
	if waitErr != nil && !forcedStop {
		return result, errors.Join(fmt.Errorf("operate %s ACP: adapter exited: %w", d.name(), waitErr), cleanupErr)
	}
	if cleanupErr != nil {
		return result, fmt.Errorf("operate %s ACP: terminate process tree: %w", d.name(), cleanupErr)
	}
	if req.Sandbox == operate.SandboxWorkspaceWrite {
		result, err = applyPatchPlan(req, result, sink)
		if err != nil {
			return result, fmt.Errorf("operate %s ACP: %w", d.name(), err)
		}
	}
	if sink != nil {
		if err := sink.Event(operate.Event{At: time.Now().UTC(), Kind: operate.EventFinal, Message: strings.TrimSpace(result.FinalMessage)}); err != nil {
			return result, fmt.Errorf("operate %s ACP: persist final event: %w", d.name(), err)
		}
	}
	return result, nil
}

func waitForACPProcess(cmd *exec.Cmd, cancel context.CancelFunc) (waitErr error, forced bool, cleanupErr error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(processWait)
	defer timer.Stop()
	select {
	case waitErr = <-done:
	case <-timer.C:
		forced = true
		cancel()
		cleanupErr = terminateProcess(cmd)
		second := time.NewTimer(processWait)
		defer second.Stop()
		select {
		case waitErr = <-done:
		case <-second.C:
			return nil, true, errors.Join(cleanupErr, errors.New("ACP adapter did not terminate after process-tree cancellation"))
		}
	}
	cleanupErr = errors.Join(cleanupErr, terminateProcess(cmd))
	return waitErr, forced, cleanupErr
}

// Close is present for the Driver contract. Each turn owns and closes its ACP
// subprocess, so the driver has no long-lived resource.
func (*Driver) Close() error { return nil }

func (d *Driver) runner() Runner {
	if d.Runner != nil {
		return d.Runner
	}
	return defaultRunner{}
}

func (d *Driver) name() string {
	if strings.TrimSpace(d.Name) != "" {
		return strings.TrimSpace(d.Name)
	}
	return "agent"
}

func (d *Driver) bin() string {
	if strings.TrimSpace(d.Bin) != "" {
		return strings.TrimSpace(d.Bin)
	}
	return d.name() + "-agent-acp"
}

func installCommand(bin string) string {
	switch bin {
	case "claude-agent-acp":
		return "npm install -g @agentclientprotocol/claude-agent-acp"
	case "codex-acp":
		return "npm install -g @agentclientprotocol/codex-acp"
	default:
		return "install " + bin + " and add it to PATH"
	}
}

type boundedCapture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	remaining int
	truncated bool
}

func newBoundedCapture(limit int) *boundedCapture { return &boundedCapture{remaining: limit} }

func (b *boundedCapture) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := len(data)
	keep := min(len(data), b.remaining)
	if keep > 0 {
		_, _ = b.buf.Write(data[:keep])
		b.remaining -= keep
	}
	if keep != len(data) {
		b.truncated = true
	}
	return total, nil
}

func (b *boundedCapture) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *boundedCapture) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

var _ io.Writer = (*boundedCapture)(nil)
