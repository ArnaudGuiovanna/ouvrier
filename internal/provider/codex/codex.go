// Package codex implements provider.Provider over official Codex transports,
// billed to the user's ChatGPT subscription. Provider uses `codex exec` as a
// legacy text-only transport. AppServerProvider uses `codex app-server` and
// surfaces structured dynamic tool calls for execution by Ouvrier. Neither
// transport reads or handles ChatGPT tokens directly.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type Runner interface {
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath(file string) (string, error)
}

type defaultRunner struct{}

func (defaultRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
func (defaultRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

// Provider drives `codex exec` as a text completion provider.
type Provider struct {
	Runner Runner
	Bin    string
	Model  string
}

func New(model string) *Provider {
	return &Provider{Runner: defaultRunner{}, Bin: "codex", Model: model}
}

func (p *Provider) Name() string { return "codex" }

func (p *Provider) runner() Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return defaultRunner{}
}

func (p *Provider) bin() string {
	if strings.TrimSpace(p.Bin) != "" {
		return p.Bin
	}
	return "codex"
}

func (p *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	return p.run(ctx, req, nil)
}

func (p *Provider) CompleteStream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	return p.run(ctx, req, onDelta)
}

func (p *Provider) run(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r := p.runner()
	bin := p.bin()
	if _, err := r.LookPath(bin); err != nil {
		return provider.Response{}, fmt.Errorf("codex provider: %s not found on PATH", bin)
	}
	model := modelName(req.Model, p.Model)
	// --skip-git-repo-check lets the worker factory run codex outside a trusted
	// git repo (the cockpit may run from any directory); read-only sandbox keeps
	// the transport side-effect free (Ouvrier governs its own tools).
	args := []string{"exec", "--json", "--color", "never", "--sandbox", "read-only", "--skip-git-repo-check"}
	if model != "" {
		args = append(args, "-m", model)
	}
	cmd := r.CommandContext(runCtx, bin, args...)
	cmd.Stdin = strings.NewReader(renderPrompt(req))
	// A worker .env may already be loaded into the parent process. The Codex
	// transport receives only the small process/auth/runtime allowlist shared
	// with app-server, never arbitrary worker or provider credentials.
	cmd.Env = sanitizedCodexEnvironment(os.Environ())
	output := newCodexExecOutput(cancel, onDelta)
	stderr := newBoundedBuffer(maxCodexExecStderrBytes)
	cmd.Stdout = output
	cmd.Stderr = stderr
	if err := configureCodexExecProcess(cmd); err != nil {
		return provider.Response{}, fmt.Errorf("codex provider: configure process containment: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return provider.Response{}, fmt.Errorf("codex provider: start: %w", err)
	}
	waitErr := cmd.Wait()
	flushErr := output.Flush()
	cleanupErr := terminateCodexExecProcess(cmd)
	response := provider.Response{Text: output.Text()}
	outputErr := output.Err()
	if outputErr == nil {
		outputErr = flushErr
	}
	if outputErr != nil {
		return response, errors.Join(fmt.Errorf("codex provider: stream output: %w", outputErr), cleanupErr)
	}
	if waitErr != nil {
		diagnostic := strings.TrimSpace(stderr.String())
		if diagnostic != "" {
			waitErr = fmt.Errorf("codex provider: %w: %s", waitErr, diagnostic)
		} else {
			waitErr = fmt.Errorf("codex provider: %w", waitErr)
		}
		return response, errors.Join(waitErr, cleanupErr)
	}
	if cleanupErr != nil {
		return response, fmt.Errorf("codex provider: terminate process tree: %w", cleanupErr)
	}
	response.StopReason = provider.StopEndTurn
	return response, nil
}

func renderPrompt(req provider.Request) string {
	var b strings.Builder
	if s := strings.TrimSpace(req.System); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	for _, m := range req.Messages {
		role := strings.ToUpper(string(m.Role))
		if t := strings.TrimSpace(m.Text()); t != "" {
			fmt.Fprintf(&b, "%s: %s\n", role, t)
		}
	}
	return b.String()
}

// modelName resolves the model to pass to `codex exec -m`. An empty result means
// "omit -m" so Codex uses the user's ~/.codex/config.toml default — required
// because account-specific models (e.g. a forced "gpt-5-codex") are rejected for
// ChatGPT-account subscriptions ("model is not supported when using Codex with a
// ChatGPT account"). Only an explicit override (e.g. codex/o3) passes -m.
func modelName(reqModel, fallback string) string {
	m := strings.TrimSpace(reqModel)
	if i := strings.IndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if m == "" {
		m = strings.TrimSpace(fallback)
	}
	switch m {
	case "", "codex", "default":
		return "" // use the account's configured default model
	}
	return m
}

func agentTextFromJSONL(line string) (string, error) {
	if strings.TrimSpace(line) == "" {
		return "", nil
	}
	var value any
	if err := json.Unmarshal([]byte(line), &value); err != nil {
		return "", fmt.Errorf("invalid JSONL: %w", err)
	}
	msg, ok := value.(map[string]any)
	if !ok {
		return "", nil
	}
	if item, ok := msg["item"].(map[string]any); ok {
		if t, ok := item["text"].(string); ok {
			return strings.TrimSpace(t), nil
		}
	}
	return "", nil
}
