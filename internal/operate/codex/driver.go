package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

// Runner starts external commands. Tests substitute it so no real Codex
// installation is required.
type Runner interface {
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath(file string) (string, error)
}

type defaultRunner struct{}

func (defaultRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func (defaultRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Driver is an operate.AgentDriver backed by the local Codex CLI.
type Driver struct {
	Runner Runner
	Bin    string
}

// New returns a Codex CLI driver.
func New() *Driver { return &Driver{Runner: defaultRunner{}, Bin: "codex"} }

// Probe checks whether the Codex binary is available and reports its version
// when possible. Authentication is owned by Codex and is only proven during a
// real turn, so Authenticated remains false here.
func (d *Driver) Probe(ctx context.Context) (operate.Capabilities, error) {
	r := d.runner()
	bin := d.bin()
	path, err := r.LookPath(bin)
	if err != nil {
		return operate.Capabilities{}, fmt.Errorf("operate codex: %s not found on PATH; install Codex and run `codex login`", bin)
	}
	caps := operate.Capabilities{Name: "codex", Transport: "exec", Authenticated: false}
	cmd := r.CommandContext(ctx, path, "--version")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err == nil {
		caps.Version = strings.TrimSpace(out.String())
	}
	return caps, nil
}

// RunTurn runs one Codex exec turn and streams normalized events into sink.
func (d *Driver) RunTurn(ctx context.Context, req operate.TurnRequest, sink operate.EventSink) (operate.TurnResult, error) {
	r := d.runner()
	bin := d.bin()
	path, err := r.LookPath(bin)
	if err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate codex: %s not found on PATH; install Codex and run `codex login`", bin)
	}

	prompt := req.Prompt
	if len(req.ContextFiles) > 0 {
		prompt += "\n\nRelevant files:\n- " + strings.Join(req.ContextFiles, "\n- ")
	}
	args, cleanup, err := execArgs(req, prompt)
	if err != nil {
		return operate.TurnResult{}, err
	}
	defer cleanup()

	cmd := r.CommandContext(ctx, path, args...)
	cmd.Dir = req.CWD
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate codex: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate codex: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return operate.TurnResult{}, fmt.Errorf("operate codex: start: %w", err)
	}

	var mu sync.Mutex
	var raw []string
	var final strings.Builder
	addRaw := func(line string) {
		mu.Lock()
		raw = append(raw, line)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			addRaw(line)
			event, text := normalizeJSONL(line)
			if text != "" {
				mu.Lock()
				if final.Len() > 0 {
					final.WriteByte('\n')
				}
				final.WriteString(text)
				mu.Unlock()
			}
			if sink != nil {
				sink.Event(event)
			}
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
		for scanner.Scan() {
			msg := scanner.Text()
			addRaw(msg)
			if sink != nil {
				sink.Event(operate.Event{At: time.Now().UTC(), Kind: operate.EventWarning, Message: msg})
			}
		}
	}()
	wg.Wait()
	err = cmd.Wait()
	if err != nil {
		return operate.TurnResult{FinalMessage: final.String(), RawOutput: strings.Join(raw, "\n")}, mapCodexErr(err, strings.Join(raw, "\n"))
	}
	return operate.TurnResult{FinalMessage: strings.TrimSpace(final.String()), RawOutput: strings.Join(raw, "\n")}, nil
}

// Close releases resources. The exec transport owns no long-lived process.
func (d *Driver) Close() error { return nil }

func (d *Driver) runner() Runner {
	if d.Runner != nil {
		return d.Runner
	}
	return defaultRunner{}
}

func (d *Driver) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "codex"
}

func execArgs(req operate.TurnRequest, prompt string) ([]string, func(), error) {
	args := []string{"exec", "--json", "--sandbox", sandboxArg(req.Sandbox)}
	cleanup := func() {}
	if strings.TrimSpace(req.OutputSchema) != "" {
		tmp, err := os.CreateTemp("", "ouvrier-codex-schema-*.json")
		if err != nil {
			return nil, cleanup, fmt.Errorf("operate codex: create schema temp file: %w", err)
		}
		if _, err := tmp.WriteString(req.OutputSchema); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, cleanup, fmt.Errorf("operate codex: write schema temp file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmp.Name())
			return nil, cleanup, fmt.Errorf("operate codex: close schema temp file: %w", err)
		}
		cleanup = func() { _ = os.Remove(tmp.Name()) }
		args = append(args, "--output-schema", tmp.Name())
	}
	args = append(args, prompt)
	return args, cleanup, nil
}

func sandboxArg(mode operate.SandboxMode) string {
	switch mode {
	case operate.SandboxWorkspaceWrite:
		return "workspace-write"
	default:
		return "read-only"
	}
}

func normalizeJSONL(line string) (operate.Event, string) {
	event := operate.Event{At: time.Now().UTC(), Kind: operate.EventAgentDelta, Message: line}
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return event, ""
	}
	typ, _ := msg["type"].(string)
	switch typ {
	case "item.started":
		event.Kind = operate.EventCommandStarted
	case "item.completed":
		event.Kind = operate.EventCommandFinished
		if item, _ := msg["item"].(map[string]any); item != nil {
			if text, _ := item["text"].(string); text != "" {
				event.Kind = operate.EventFinal
				event.Message = text
				return event, text
			}
			if command, _ := item["command"].(string); command != "" {
				event.Command = command
			}
		}
	case "turn.completed":
		event.Kind = operate.EventFinal
	case "turn.failed", "error":
		event.Kind = operate.EventError
		if message, _ := msg["message"].(string); message != "" {
			event.Message = message
		}
	}
	if message, _ := msg["message"].(string); message != "" {
		event.Message = message
	}
	return event, ""
}

func mapCodexErr(err error, output string) error {
	lower := strings.ToLower(output)
	if strings.Contains(lower, "auth") || strings.Contains(lower, "login") || strings.Contains(lower, "unauthorized") {
		return fmt.Errorf("operate codex: authentication failed; run `codex login` or your organization-approved Codex login flow: %w", err)
	}
	return fmt.Errorf("operate codex: %w", err)
}
