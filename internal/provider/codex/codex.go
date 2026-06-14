// Package codex implements provider.Provider over the official `codex exec`
// transport, billed to the user's ChatGPT subscription. It is a TEXT model:
// codex runs its own tools, so structured Ouvrier tool-calls are not surfaced
// here (use an API-key provider for native tool-use). We never read tokens.
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	r := p.runner()
	bin := p.bin()
	if _, err := r.LookPath(bin); err != nil {
		return provider.Response{}, fmt.Errorf("codex provider: %s not found on PATH", bin)
	}
	model := modelName(req.Model, p.Model)
	args := []string{"exec", "--json", "--color", "never", "--sandbox", "read-only"}
	if model != "" {
		args = append(args, "-m", model)
	}
	cmd := r.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(renderPrompt(req))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return provider.Response{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return provider.Response{}, err
	}
	var text strings.Builder
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if chunk := agentTextFromJSONL(sc.Text()); chunk != "" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(chunk)
			if onDelta != nil {
				onDelta(provider.Delta{Text: chunk})
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return provider.Response{Text: strings.TrimSpace(text.String())}, fmt.Errorf("codex provider: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return provider.Response{Text: strings.TrimSpace(text.String()), StopReason: provider.StopEndTurn}, nil
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

func modelName(reqModel, fallback string) string {
	m := strings.TrimSpace(reqModel)
	if i := strings.IndexByte(m, '/'); i >= 0 {
		m = m[i+1:]
	}
	if m == "" {
		m = strings.TrimSpace(fallback)
	}
	return m
}

func agentTextFromJSONL(line string) string {
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return ""
	}
	if item, ok := msg["item"].(map[string]any); ok {
		if t, ok := item["text"].(string); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}
