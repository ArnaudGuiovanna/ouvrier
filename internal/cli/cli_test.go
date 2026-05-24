package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

func TestRunVersionPrintsConfiguredVersion(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("0.1.0", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"version"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if got, want := out.String(), "ouvrier 0.1.0\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNewHelpPrintsUsage(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"new", "--help"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Usage: ouvrier new",
		"Bubble Tea",
		"preview",
		"HTTP trigger",
		"--help",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("new help missing %q in:\n%s", want, got)
		}
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNewWithoutFlagsUsesTUIRunner(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	called := false
	app := New("dev", WithStreams(strings.NewReader(""), &out, &errOut))
	app.runNew = func(_ io.Reader, _ io.Writer) error {
		called = true
		return nil
	}

	err := app.Run(context.Background(), []string{"new"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if !called {
		t.Fatal("new without flags did not call TUI runner")
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNewWithFlagsScaffoldsProject(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	parent := t.TempDir()
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{
		"new",
		"--name", "demo",
		"--trigger", "POST /tickets",
		"--model", "anthropic/claude-sonnet-4-6",
		"--yes",
		"--dir", parent,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	projectDir := filepath.Join(parent, "demo")
	for _, path := range []string{"main.go", "go.mod", "pip.yaml", ".env.example", ".gitignore", "README.md"} {
		if _, err := os.Stat(filepath.Join(projectDir, path)); err != nil {
			t.Fatalf("generated file %q missing: %v", path, err)
		}
	}
	if got := out.String(); !strings.Contains(got, "created "+projectDir) {
		t.Fatalf("stdout = %q, want created project path", got)
	}
	if got := errOut.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunNewWithFlagsRejectsNonHTTPTriggerClearly(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	parent := t.TempDir()
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{
		"new",
		"--name", "demo",
		"--trigger", "kafka://tickets",
		"--model", "anthropic/claude-sonnet-4-6",
		"--yes",
		"--dir", parent,
	})
	if !errors.Is(err, scaffold.ErrInvalidConfig) {
		t.Fatalf("Run() error = %v, want ErrInvalidConfig", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if got := errOut.String(); !strings.Contains(got, "--trigger accepts only HTTP routes") {
		t.Fatalf("stderr = %q, want HTTP-only trigger guidance", got)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "demo")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("project directory stat error = %v, want os.ErrNotExist", statErr)
	}
}

func TestRunNewWithFlagsRequiresYes(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{
		"new",
		"--name", "demo",
		"--trigger", "POST /tickets",
		"--model", "anthropic/claude-sonnet-4-6",
		"--dir", t.TempDir(),
	})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if got := errOut.String(); !strings.Contains(got, "--yes") {
		t.Fatalf("stderr = %q, want --yes guidance", got)
	}
}

func TestRunUnknownCommandReturnsUsageError(t *testing.T) {
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	err := app.Run(context.Background(), []string{"missing"})
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("Run() error = %v, want ErrUnknownCommand", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("stdout = %q, want empty", got)
	}
	if got := errOut.String(); !strings.Contains(got, `unknown command "missing"`) {
		t.Fatalf("stderr = %q, want unknown command", got)
	}
}
