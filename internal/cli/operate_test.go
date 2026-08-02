package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui"
)

func TestRunOperateHelpListsWorkflowCommands(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	if err := app.run(context.Background(), []string{"operate", "--help"}); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	for _, want := range []string{
		"ouvrier operate patch",
		"ouvrier operate fix-worker",
		"ouvrier operate audit",
		"ouvrier operate transfer",
		"--allow-failed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q in:\n%s", want, out.String())
		}
	}
}

func TestRunOperateWithoutSubcommandUsesBubbleTeaRunner(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(strings.NewReader(""), &out, &errOut), WithSignedIn(func() bool { return false }))
	called := false
	app.runOperate = func(_ context.Context, _ io.Reader, _ io.Writer, opts tui.OperateOptions) error {
		called = true
		if opts.Agent != "manual" || opts.Goal != "add tool" {
			t.Fatalf("opts = %+v", opts)
		}
		if opts.Driver == nil {
			t.Fatal("opts.Driver = nil, want driver")
		}
		return nil
	}

	err := app.run(context.Background(), []string{"operate", "--agent", "manual", "--goal", "add tool"})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !called {
		t.Fatal("operate did not call Bubble Tea runner")
	}
}

func TestRunOperatePrintPromptFailsClosedWithoutAutoSafe(t *testing.T) {
	parent := t.TempDir()
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut), WithSignedIn(func() bool { return false }))

	err := app.run(context.Background(), []string{
		"operate",
		"--agent", "manual",
		"--dir", parent,
		"--print",
		"Create a worker that receives POST /tickets",
	})
	if !errors.Is(err, operate.ErrToolDenied) {
		t.Fatalf("run() error = %v, want ErrToolDenied", err)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "ticket-worker")); !os.IsNotExist(statErr) {
		t.Fatalf("headless default created worker; stat error = %v", statErr)
	}
	if !strings.Contains(out.String(), "skipped scaffold_worker") {
		t.Fatalf("stdout does not expose fail-closed denial:\n%s", out.String())
	}
}

func TestRunOperateJSONPrompt(t *testing.T) {
	parent := t.TempDir()
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut), WithSignedIn(func() bool { return false }))

	err := app.run(context.Background(), []string{
		"operate",
		"--agent", "manual",
		"--dir", parent,
		"--mode", "json",
		"--prompt", "/tools",
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), `"session_id"`) || !strings.Contains(out.String(), "Available Ouvrier tools") {
		t.Fatalf("stdout = %q, want JSON turn", out.String())
	}
}

func TestRunOperateRPCPrompt(t *testing.T) {
	parent := t.TempDir()
	var out bytes.Buffer
	var errOut bytes.Buffer
	input := strings.NewReader(
		`{"type":"prompt","text":"/tools"}` + "\n" +
			`{"type":"follow_up","text":"/policy"}` + "\n" +
			`{"type":"compact"}` + "\n",
	)
	app := New("dev", WithStreams(input, &out, &errOut), WithSignedIn(func() bool { return false }))

	err := app.run(context.Background(), []string{
		"operate",
		"--agent", "manual",
		"--dir", parent,
		"--mode", "rpc",
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), `"type":"turn"`) || !strings.Contains(out.String(), "Available Ouvrier tools") || !strings.Contains(out.String(), `"context_compaction":true`) {
		t.Fatalf("stdout = %q, want rpc turn", out.String())
	}
}

func TestOperatePrintJSONAndRPCRedactProductionEnvironmentSecrets(t *testing.T) {
	const secret = "cli-production-secret-value"
	t.Setenv("OUVRIER_ADMIN_TOKEN", secret)

	tests := []struct {
		name  string
		input string
		args  func(string) []string
	}{
		{
			name: "print",
			args: func(dir string) []string {
				return []string{"operate", "--agent", "manual", "--dir", dir, "--print", "/policy " + secret}
			},
		},
		{
			name: "json",
			args: func(dir string) []string {
				return []string{"operate", "--agent", "manual", "--dir", dir, "--mode", "json", "--prompt", "/policy " + secret}
			},
		},
		{
			name:  "rpc",
			input: `{"type":"prompt","text":"/policy ` + secret + `"}` + "\n",
			args: func(dir string) []string {
				return []string{"operate", "--agent", "manual", "--dir", dir, "--mode", "rpc"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			var errOut bytes.Buffer
			app := New("dev", WithStreams(strings.NewReader(test.input), &out, &errOut), WithSignedIn(func() bool { return false }))
			if err := app.run(context.Background(), test.args(t.TempDir())); err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
				t.Fatalf("operate %s leaked secret; stdout=%q stderr=%q", test.name, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), "***") {
				t.Fatalf("operate %s output has no redaction marker: %q", test.name, out.String())
			}
		})
	}
}

func TestRunOperateReviewWorkerManualCreatesReviewSession(t *testing.T) {
	dir := writeOperateWorkerFixture(t)
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.run(context.Background(), []string{"operate", "review-worker", "--agent", "manual", "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "review failed") {
		t.Fatalf("run() error = %v, want failed-review exit", err)
	}
	if !strings.Contains(out.String(), "reviewed demo") || !strings.Contains(out.String(), "manual mode review") {
		t.Fatalf("stdout = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".ouvrier", "operate", "sessions")); err != nil {
		t.Fatalf("session dir missing: %v", err)
	}
	reviews, err := filepath.Glob(filepath.Join(dir, ".ouvrier", "operate", "sessions", "*", "review.json"))
	if err != nil || len(reviews) != 1 {
		t.Fatalf("persisted review artifacts = %v, %v", reviews, err)
	}
	reviewData, err := os.ReadFile(reviews[0])
	if err != nil {
		t.Fatalf("read persisted review: %v", err)
	}
	if !strings.Contains(string(reviewData), `"passed": false`) || !strings.Contains(string(reviewData), "manual mode review") {
		t.Fatalf("persisted failed review = %s", reviewData)
	}
}

func TestRunOperatePatchManualPersistsDiffArtifact(t *testing.T) {
	dir := writeOperateWorkerFixture(t)
	gitInitAndCommitOperateFixture(t, dir)
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.run(context.Background(), []string{"operate", "patch", "--agent", "manual", "--dir", dir, "--goal", "add a readonly tool"})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(out.String(), "patched demo") || !strings.Contains(out.String(), "diff:") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func gitInitAndCommitOperateFixture(t *testing.T, dir string) {
	t.Helper()
	commands := [][]string{
		{"git", "init"},
		{"git", "add", "."},
		{"git", "-c", "user.name=Ouvrier Test", "-c", "user.email=test@example.com", "commit", "-m", "initial"},
	}
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestRunOperateCreateWorkerUsesScaffold(t *testing.T) {
	parent := t.TempDir()
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.run(context.Background(), []string{
		"operate", "create-worker",
		"--yes",
		"--name", "demo",
		"--trigger", "POST /tickets",
		"--model", "anthropic/claude-sonnet-4-6",
		"--dir", parent,
	})
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "demo", "pip.yaml")); err != nil {
		t.Fatalf("created worker missing pip.yaml: %v", err)
	}
	if !strings.Contains(out.String(), "next: cd ") || !strings.Contains(out.String(), "ouvrier operate") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestRunOperateBuildRequiresPassingAudit(t *testing.T) {
	dir := writeOperateWorkerFixture(t)
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.run(context.Background(), []string{"operate", "build", "--dir", dir})
	if err == nil || !strings.Contains(err.Error(), "requires a passing audit") {
		t.Fatalf("run() error = %v, want audit gate", err)
	}
	if errors.Is(err, ErrUsage) {
		t.Fatalf("run() error = %v, should be workflow gate not usage", err)
	}
}

func writeOperateWorkerFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("pip.yaml", `name: demo
version: 0.1.0
deploy:
  staging:
    hosts: [deploy@example.com]
`)
	write("main.go", "package main\n\nfunc main() {}\n")
	write("ouvrier.worker.json", `{"name":"demo","events":["POST /tickets"],"outcomes":["triage"],"admin_url":"http://127.0.0.1:8080"}`+"\n")
	return dir
}
