package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	authpkg "github.com/ArnaudGuiovanna/ouvrier/internal/auth"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
)

func TestAgentsListsCodexAndClaudeReadiness(t *testing.T) {
	out := &bytes.Buffer{}
	app := New("test",
		WithStreams(nil, out, &bytes.Buffer{}),
		WithSignedIn(func() bool { return true }),
		withAgentDiscovery(
			func(context.Context) (authpkg.AuthState, string) { return authpkg.StateAuthed, "claude.ai" },
			func(name string) (string, error) {
				switch name {
				case "codex", "codex-acp", "claude", "claude-agent-acp":
					return "/usr/bin/" + name, nil
				default:
					return "", errors.New("missing")
				}
			},
		),
	)
	if err := app.run(context.Background(), []string{"agents"}); err != nil {
		t.Fatalf("agents: %v", err)
	}
	text := out.String()
	for _, want := range []string{"codex", "claude", "acp/v1", "ready"} {
		if !strings.Contains(text, want) {
			t.Fatalf("agents output missing %q:\n%s", want, text)
		}
	}
}

func TestAgentsJSONExplainsMissingClaudeAdapter(t *testing.T) {
	out := &bytes.Buffer{}
	app := New("test",
		WithStreams(nil, out, &bytes.Buffer{}),
		WithSignedIn(func() bool { return false }),
		withAgentDiscovery(
			func(context.Context) (authpkg.AuthState, string) { return authpkg.StateAuthed, "claude.ai" },
			func(name string) (string, error) {
				if name == "claude" {
					return "/usr/bin/claude", nil
				}
				return "", errors.New("missing")
			},
		),
	)
	if err := app.run(context.Background(), []string{"agents", "--json"}); err != nil {
		t.Fatalf("agents --json: %v", err)
	}
	var rows []agentStatus
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("decode agents JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 2 || rows[1].ID != "claude" || rows[1].Ready || rows[1].Adapter {
		t.Fatalf("agent rows = %+v", rows)
	}
	if !strings.Contains(rows[1].Detail, "ACP adapter not detected") {
		t.Fatalf("missing install guidance: %+v", rows[1])
	}
}

func TestDefaultAgentLookPathFindsManagedAdapterDirectory(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex-acp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUVRIER_ACP_BIN_DIR", dir)
	got, err := defaultAgentLookPath("codex-acp")
	if err != nil || got != bin {
		t.Fatalf("defaultAgentLookPath() = %q, %v; want %q", got, err, bin)
	}
}

func TestRootForwardsAgentFlagToCockpit(t *testing.T) {
	app := New("test",
		WithStreams(nil, &bytes.Buffer{}, &bytes.Buffer{}),
		WithSignedIn(func() bool { return true }),
		withAgentDiscovery(nil, func(name string) (string, error) {
			if name == "codex" || name == "codex-acp" {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("missing")
		}),
	)
	called := false
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, opts tui.OperateOptions) error {
		called = true
		if opts.Agent != "codex" || opts.AgentTransport != "acp/v1" {
			t.Fatalf("operate options = %+v", opts)
		}
		return nil
	}
	if err := app.run(context.Background(), []string{"--agent", "codex"}); err != nil {
		t.Fatalf("root --agent: %v", err)
	}
	if !called {
		t.Fatal("root --agent did not launch cockpit")
	}
}

func TestRootAutoLaunchesDetectedAgentPicker(t *testing.T) {
	app := New("test",
		WithStreams(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}),
		WithSignedIn(func() bool { return true }),
		withAgentDiscovery(
			func(context.Context) (authpkg.AuthState, string) { return authpkg.StateAuthed, "claude.ai" },
			func(name string) (string, error) { return "/usr/bin/" + name, nil },
		),
	)
	app.interactive = func(io.Reader, io.Writer) bool { return true }
	app.runAgentPicker = func(_ context.Context, _ io.Reader, _ io.Writer, choices []tui.AgentChoice) (string, error) {
		if len(choices) != 2 || choices[0].ID != "codex" || !choices[0].Ready || choices[1].ID != "claude" || !choices[1].Ready {
			t.Fatalf("picker choices = %+v", choices)
		}
		return "claude", nil
	}
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, opts tui.OperateOptions) error {
		if opts.Agent != "claude" || opts.AgentTransport != "acp/v1" {
			t.Fatalf("operate options = %+v", opts)
		}
		return nil
	}
	if err := app.run(context.Background(), nil); err != nil {
		t.Fatalf("root cockpit: %v", err)
	}
}
