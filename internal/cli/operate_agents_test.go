package cli

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
)

func TestSelectOperateAgentAutoPrefersAuthenticatedCodex(t *testing.T) {
	claudeCalls := 0
	selected, err := selectOperateAgent(context.Background(), "auto", "auto",
		func() bool { return true },
		func(context.Context) (authpkg.AuthState, string) {
			claudeCalls++
			return authpkg.StateAuthed, "claude.ai"
		},
		func(name string) (string, error) { return "/usr/bin/" + name, nil },
	)
	if err != nil || selected.ID != "codex" || selected.Transport != "acp/v1" || selected.AuthState != "authed" {
		t.Fatalf("select auto = %+v, %v", selected, err)
	}
	if claudeCalls != 0 {
		t.Fatalf("Claude was probed despite available preferred Codex: %d calls", claudeCalls)
	}
}

func TestSelectOperateAgentAutoFallsBackToAuthenticatedClaudeACP(t *testing.T) {
	selected, err := selectOperateAgent(context.Background(), "auto", "auto",
		func() bool { return false },
		func(context.Context) (authpkg.AuthState, string) { return authpkg.StateAuthed, "claude.ai" },
		func(name string) (string, error) {
			switch name {
			case "codex", "codex-acp":
				return "", exec.ErrNotFound
			case "claude-agent-acp":
				return "/usr/bin/claude-agent-acp", nil
			default:
				t.Fatalf("LookPath(%q)", name)
			}
			return "", exec.ErrNotFound
		},
	)
	if err != nil || selected.ID != "claude" || selected.Transport != "acp/v1" || selected.AuthAccount != "claude.ai" {
		t.Fatalf("select auto = %+v, %v", selected, err)
	}
}

func TestSelectOperateAgentClaudeRequiresCanonicalACPAdapter(t *testing.T) {
	selected, err := selectOperateAgent(context.Background(), "claude", "auto",
		func() bool { return false },
		func(context.Context) (authpkg.AuthState, string) { return authpkg.StateAuthed, "claude.ai" },
		func(string) (string, error) { return "", exec.ErrNotFound },
	)
	if err == nil || selected.ID != "" || !strings.Contains(err.Error(), "claude-agent-acp") || !strings.Contains(err.Error(), "ACP adapter") {
		t.Fatalf("select claude = %+v, %v", selected, err)
	}
}

func TestSelectOperateAgentCodexRequiresACPAdapterByDefault(t *testing.T) {
	selected, err := selectOperateAgent(context.Background(), "codex", "auto",
		func() bool { return true },
		func(context.Context) (authpkg.AuthState, string) { return authpkg.StateUnauthed, "" },
		func(name string) (string, error) {
			if name == "codex" {
				return "/usr/bin/codex", nil
			}
			return "", exec.ErrNotFound
		},
	)
	if err == nil || selected.ID != "" || !strings.Contains(err.Error(), "codex-acp") || !strings.Contains(err.Error(), "ACP adapter") {
		t.Fatalf("select codex = %+v, %v", selected, err)
	}
}

func TestSelectOperateAgentExplicitAppServerDoesNotRequireACPAdapter(t *testing.T) {
	selected, err := selectOperateAgent(context.Background(), "codex", "app-server",
		func() bool { return true },
		func(context.Context) (authpkg.AuthState, string) { return authpkg.StateUnauthed, "" },
		func(name string) (string, error) {
			if name == "codex" {
				return "/usr/bin/codex", nil
			}
			return "", exec.ErrNotFound
		},
	)
	if err != nil || selected.ID != "codex" || selected.Transport != "app-server" {
		t.Fatalf("select explicit app-server = %+v, %v", selected, err)
	}
}

func TestSelectOperateAgentRequiresAuthentication(t *testing.T) {
	_, err := selectOperateAgent(context.Background(), "auto", "auto",
		func() bool { return false },
		func(context.Context) (authpkg.AuthState, string) { return authpkg.StateUnauthed, "" },
		func(string) (string, error) { return "", errors.New("missing") },
	)
	if err == nil || !strings.Contains(err.Error(), "saved session") || strings.Contains(err.Error(), " login") {
		t.Fatalf("select auto error = %v", err)
	}
}

func TestResolveSelectedAgentModelDoesNotSilentlyUseCodexForClaude(t *testing.T) {
	model, id, err := resolveSelectedAgentModel("claude", "", "auto", t.TempDir(), func() bool { return true })
	if err != nil || model != nil || id != "claude/acp" {
		t.Fatalf("resolve Claude model = %T, %q, %v", model, id, err)
	}
}

func TestResolveSelectedAgentModelUsesACPForCodexAuto(t *testing.T) {
	model, id, err := resolveSelectedAgentModel("codex", "", "auto", t.TempDir(), func() bool { return true })
	if err != nil || model != nil || id != "codex/acp" {
		t.Fatalf("resolve Codex ACP model = %T, %q, %v", model, id, err)
	}
}

func TestResolveOperateAgentRewritesAutoBeforeDriverConstruction(t *testing.T) {
	app := New("test",
		WithSignedIn(func() bool { return true }),
		withAgentDiscovery(nil, func(name string) (string, error) {
			if name == "codex" || name == "codex-acp" {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		}),
	)
	cfg, selected, err := app.resolveOperateAgent(context.Background(), operateConfig{Agent: "auto", CodexMode: "auto"})
	if err != nil {
		t.Fatalf("resolve auto agent: %v", err)
	}
	if cfg.Agent != "codex" || selected.ID != "codex" {
		t.Fatalf("resolved config = %+v, selection = %+v", cfg, selected)
	}
	driver, id, _, err := operateDriver(cfg)
	if err != nil {
		t.Fatalf("construct resolved driver: %v", err)
	}
	defer driver.Close()
	if id != "codex" {
		t.Fatalf("driver id = %q, want codex", id)
	}
}
