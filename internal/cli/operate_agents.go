package cli

import (
	"context"
	"fmt"
	"strings"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
)

type selectedOperateAgent struct {
	ID          string
	Transport   string
	AdapterPath string
	AuthState   string
	AuthAccount string
}

type claudeAuthProbe func(context.Context) (authpkg.AuthState, string)
type agentLookPath func(string) (string, error)

func defaultClaudeAuthProbe(ctx context.Context) (authpkg.AuthState, string) {
	return (&authpkg.Claude{}).Probe(ctx)
}

func (app *App) resolveOperateAgent(ctx context.Context, cfg operateConfig) (operateConfig, selectedOperateAgent, error) {
	selected, err := selectOperateAgent(ctx, cfg.Agent, cfg.CodexMode, app.signedIn, app.claudeAuth, app.agentPath)
	if err != nil {
		return operateConfig{}, selectedOperateAgent{}, err
	}
	cfg.Agent = selected.ID
	cfg.AgentBin = selected.AdapterPath
	return cfg, selected, nil
}

func selectOperateAgent(
	ctx context.Context,
	requested string,
	codexMode string,
	codexAuthed func() bool,
	probeClaude claudeAuthProbe,
	lookPath agentLookPath,
) (selectedOperateAgent, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		requested = "auto"
	}
	if requested == "manual" {
		return selectedOperateAgent{ID: "manual", Transport: "none", AuthState: "authed"}, nil
	}
	if codexAuthed == nil {
		codexAuthed = func() bool { return false }
	}
	if probeClaude == nil {
		probeClaude = defaultClaudeAuthProbe
	}
	if lookPath == nil {
		lookPath = defaultAgentLookPath
	}

	if requested == "codex" || requested == "auto" {
		transport := codexAgentTransport(codexMode)
		_, cliErr := lookPath("codex")
		adapterReady := true
		adapterPath := ""
		if transport == "acp/v1" {
			var adapterErr error
			adapterPath, adapterErr = lookPath("codex-acp")
			adapterReady = adapterErr == nil
		}
		authed := codexAuthed()
		if cliErr == nil && authed && adapterReady {
			return selectedOperateAgent{ID: "codex", Transport: transport, AdapterPath: adapterPath, AuthState: "authed", AuthAccount: "ChatGPT"}, nil
		}
		if requested == "codex" {
			switch {
			case cliErr != nil:
				return selectedOperateAgent{}, fmt.Errorf("%w: Codex CLI is not available", ErrUsage)
			case !authed:
				return selectedOperateAgent{}, fmt.Errorf("%w: saved Codex session is unavailable; open Codex once to complete its own sign-in", ErrUsage)
			default:
				return selectedOperateAgent{}, fmt.Errorf("%w: Codex ACP adapter `codex-acp` is not available", ErrUsage)
			}
		}
	}

	if requested == "claude" || requested == "auto" {
		probeCtx := ctx
		if probeCtx == nil {
			probeCtx = context.Background()
		}
		probeCtx, cancel := context.WithTimeout(probeCtx, defaultCodexAuthProbeTimeout)
		state, account := probeClaude(probeCtx)
		cancel()
		if state == authpkg.StateAuthed {
			adapterPath, err := lookPath("claude-agent-acp")
			if err != nil {
				return selectedOperateAgent{}, fmt.Errorf("%w: Claude Code is ready, but its ACP adapter `claude-agent-acp` is not available", ErrUsage)
			}
			return selectedOperateAgent{ID: "claude", Transport: "acp/v1", AdapterPath: adapterPath, AuthState: "authed", AuthAccount: account}, nil
		}
		if requested == "claude" {
			switch state {
			case authpkg.StateNoClaude:
				return selectedOperateAgent{}, fmt.Errorf("%w: Claude Code CLI is not available", ErrUsage)
			default:
				return selectedOperateAgent{}, fmt.Errorf("%w: saved Claude session is unavailable; open Claude Code once to complete its own sign-in", ErrUsage)
			}
		}
	}

	return selectedOperateAgent{}, fmt.Errorf("%w: no ready ACP coding agent was detected; Codex or Claude needs an existing saved session and its ACP adapter", ErrUsage)
}

func codexAgentTransport(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "app-server":
		return "app-server"
	case "exec":
		return "exec"
	default:
		return "acp/v1"
	}
}
