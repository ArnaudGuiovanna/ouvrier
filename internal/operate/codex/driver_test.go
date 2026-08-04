package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestExecArgsUsesJSONSandboxAndSchemaFile(t *testing.T) {
	args, cleanup, err := execArgs(operate.TurnRequest{
		Sandbox:      operate.SandboxWorkspaceWrite,
		OutputSchema: `{"type":"object"}`,
	}, "do work")
	if err != nil {
		t.Fatalf("execArgs() error = %v", err)
	}
	defer cleanup()

	got := strings.Join(args, " ")
	for _, want := range []string{
		"exec", "--json", "--color never", "--ephemeral", "--ignore-user-config",
		"--ignore-rules", "--strict-config", "--skip-git-repo-check",
		`web_search="disabled"`, `project_doc_max_bytes=0`, `mcp_servers={}`,
		`features.apps=false`, `features.multi_agent=false`, `features.plugins=false`,
		`features.shell_tool=false`, `features.skill_mcp_dependency_install=false`,
		`features.unified_exec=false`, "--sandbox workspace-write", "--output-schema",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args = %v, missing %q", args, want)
		}
	}
	if args[len(args)-1] != "do work" {
		t.Fatalf("last arg = %q, want prompt", args[len(args)-1])
	}
	schemaPath := args[len(args)-2]
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("schema file missing: %v", err)
	}
	if string(data) != `{"type":"object"}` {
		t.Fatalf("schema file = %q", data)
	}
}

func TestCodexPromptContextIsBoundedRootedAndStructurallyRedacted(t *testing.T) {
	dir := t.TempDir()
	const secret = "hardcoded-secret-value"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nconst API_KEY = \""+secret+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt, err := codexPromptWithContext(operate.TurnRequest{
		CWD: dir, Prompt: "review", ContextFiles: []string{"main.go", "main.go"},
	})
	if err != nil {
		t.Fatalf("codexPromptWithContext() error = %v", err)
	}
	if strings.Contains(prompt, secret) || !strings.Contains(prompt, `API_KEY = ***`) {
		t.Fatalf("context prompt was not redacted: %q", prompt)
	}
	if strings.Count(prompt, `<worker-file path="main.go">`) != 1 {
		t.Fatalf("context prompt did not deduplicate rooted file: %q", prompt)
	}

	external := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(external, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "linked.go")); err == nil {
		if _, err := codexPromptWithContext(operate.TurnRequest{CWD: dir, ContextFiles: []string{"linked.go"}}); err == nil {
			t.Fatal("codexPromptWithContext accepted a symlink context file")
		}
	}
	if _, err := codexPromptWithContext(operate.TurnRequest{CWD: dir, ContextFiles: []string{"../secret.go"}}); err == nil {
		t.Fatal("codexPromptWithContext accepted a parent traversal")
	}
}

func TestLegacyCodexRedactsKnownSecretsFromPromptContextAndSchema(t *testing.T) {
	dir := t.TempDir()
	const secret = "known-production-secret-value"
	if err := os.MkdirAll(filepath.Join(dir, "internal", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "deep", "worker.go"), []byte("package deep\n\nconst value = \""+secret+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := operate.TurnRequest{
		CWD:          dir,
		Prompt:       "Goal and subject contain " + secret,
		ContextFiles: []string{"internal/deep/worker.go"},
		OutputSchema: `{"type":"object","description":"` + secret + `"}`,
		Redactor:     operate.NewRedactor(secret),
	}
	prompt, err := codexPromptWithContext(req)
	if err != nil {
		t.Fatalf("codexPromptWithContext() error = %v", err)
	}
	args, cleanup, err := execArgs(req, prompt)
	if err != nil {
		t.Fatalf("execArgs() error = %v", err)
	}
	defer cleanup()
	if strings.Contains(strings.Join(args, "\n"), secret) || !strings.Contains(prompt, "***") {
		t.Fatalf("Codex args leaked known secret: %v", args)
	}
	schemaPath := args[len(args)-2]
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schema), secret) || !strings.Contains(string(schema), "***") {
		t.Fatalf("Codex schema was not redacted: %q", schema)
	}
}

func TestNormalizeJSONLMapsFinalAndCommandEvents(t *testing.T) {
	event, text := normalizeJSONL(`{"type":"item.completed","item":{"text":"done"}}`)
	if event.Kind != operate.EventFinal || text != "done" {
		t.Fatalf("final event = %+v text=%q", event, text)
	}

	event, text = normalizeJSONL(`{"type":"item.completed","item":{"command":"go test ./..."}}`)
	if event.Kind != operate.EventCommandFinished || event.Command != "go test ./..." || text != "" {
		t.Fatalf("command event = %+v text=%q", event, text)
	}
}

func TestMapCodexErrMentionsLoginForAuthFailures(t *testing.T) {
	err := mapCodexErr(errors.New("exit status 1"), "Unauthorized: login required")
	if err == nil || !strings.Contains(err.Error(), "open Codex once") {
		t.Fatalf("error = %v, want command-free Codex readiness guidance", err)
	}
}

func TestCodexCommandEnvDropsWorkerProviderAndCloudSecrets(t *testing.T) {
	env := codexCommandEnv([]string{
		"PATH=/custom/bin:/usr/bin",
		"HOME=/home/operator",
		"CODEX_HOME=/home/operator/.codex",
		"LANG=fr_FR.UTF-8",
		"HTTPS_PROXY=http://proxy.example",
		"SSL_CERT_FILE=/etc/ssl/custom.pem",
		"OPENAI_API_KEY=openai-secret",
		"ANTHROPIC_API_KEY=anthropic-secret",
		"OUVRIER_ADMIN_TOKEN=worker-secret",
		"DATABASE_URL=postgres://secret",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"GOFLAGS=-toolexec=evil",
	})
	values := make(map[string]string, len(env))
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", item)
		}
		values[name] = value
	}
	for key, want := range map[string]string{
		"PATH": "/custom/bin:/usr/bin", "HOME": "/home/operator", "CODEX_HOME": "/home/operator/.codex",
		"LANG": "fr_FR.UTF-8", "HTTPS_PROXY": "http://proxy.example", "SSL_CERT_FILE": "/etc/ssl/custom.pem",
		"GOENV": "off", "GOWORK": "off",
	} {
		if got := values[key]; got != want {
			t.Fatalf("environment %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OUVRIER_ADMIN_TOKEN", "DATABASE_URL",
		"AWS_SECRET_ACCESS_KEY", "SSH_AUTH_SOCK", "GOFLAGS",
	} {
		if _, ok := values[key]; ok {
			t.Fatalf("sensitive environment %s reached Codex command", key)
		}
	}
}

func TestDriverRejectsOversizedOutputLine(t *testing.T) {
	driver := &Driver{Runner: codexHelperRunner{mode: "oversized-line"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := driver.RunTurn(ctx, operate.TurnRequest{CWD: t.TempDir(), Prompt: "test oversized output"}, nil)
	if err == nil || !strings.Contains(err.Error(), "stdout line exceeds") {
		t.Fatalf("RunTurn() error = %v, want explicit oversized-line failure", err)
	}
	if len(result.RawOutput) > maxCodexOutputBytes {
		t.Fatalf("RawOutput bytes = %d, want <= %d", len(result.RawOutput), maxCodexOutputBytes)
	}
}

func TestDriverBoundsCumulativeOutput(t *testing.T) {
	driver := &Driver{Runner: codexHelperRunner{mode: "cumulative-output"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := driver.RunTurn(ctx, operate.TurnRequest{CWD: t.TempDir(), Prompt: "test cumulative output"}, nil)
	if err == nil || !strings.Contains(err.Error(), "cumulative output exceeds") {
		t.Fatalf("RunTurn() error = %v, want explicit cumulative-output failure", err)
	}
	if len(result.RawOutput) > maxCodexOutputBytes {
		t.Fatalf("RawOutput bytes = %d, want <= %d", len(result.RawOutput), maxCodexOutputBytes)
	}
}

func TestDriverWaitDelayBoundsInheritedPipes(t *testing.T) {
	driver := &Driver{Runner: codexHelperRunner{mode: "inherited-pipe"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	result, err := driver.RunTurn(ctx, operate.TurnRequest{CWD: t.TempDir(), Prompt: "test inherited pipe"}, nil)
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "WaitDelay") {
		t.Fatalf("RunTurn() error = %v, want bounded inherited-pipe failure", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("RunTurn() elapsed = %s, inherited pipe was not bounded", elapsed)
	}
	if pid := codexHelperPID(result.RawOutput); pid > 0 {
		process, findErr := os.FindProcess(pid)
		if findErr == nil {
			t.Cleanup(func() { _ = process.Kill() })
		}
	}
}

type codexHelperRunner struct{ mode string }

func (r codexHelperRunner) CommandContext(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexDriverHelperProcess$", "--", "codex-helper="+r.mode)
}

func (codexHelperRunner) LookPath(string) (string, error) { return os.Args[0], nil }

func TestCodexDriverHelperProcess(t *testing.T) {
	mode := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "codex-helper=") {
			mode = strings.TrimPrefix(arg, "codex-helper=")
			break
		}
	}
	if mode == "" {
		return
	}
	switch mode {
	case "oversized-line":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), maxCodexLineBytes+1))
	case "cumulative-output":
		line := bytes.Repeat([]byte("x"), 64*1024)
		for range maxCodexOutputBytes/(len(line)+1) + 4 {
			_, _ = os.Stdout.Write(line)
			_, _ = os.Stdout.Write([]byte{'\n'})
		}
	case "inherited-pipe":
		child := exec.Command("sh", "-c", "sleep 6")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatalf("start inherited-pipe child: %v", err)
		}
		fmt.Fprintf(os.Stdout, "orphan_pid=%d\n", child.Process.Pid)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func codexHelperPID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "orphan_pid=")
		if !ok {
			continue
		}
		pid, _ := strconv.Atoi(value)
		return pid
	}
	return 0
}
