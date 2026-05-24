package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddToolRequiresName(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "tool"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddToolRejectsBadIdentifier(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "tool", "--name", "9bad", "--dir", dir})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddToolRejectsMutuallyExclusiveEffects(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "tool",
		"--name", "load_ticket",
		"--readonly", "--side-effecting",
		"--dir", dir,
	})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddToolCreatesStubAndRegistration(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "tool",
		"--name", "load_ticket",
		"--describe", "Load ticket by id.",
		"--readonly",
		"--dir", dir,
	})
	if err != nil {
		t.Fatalf("Run() error = %v\nstderr=%s", err, errOut.String())
	}

	stub := filepath.Join(dir, "tools", "load_ticket.go")
	stubData, readErr := os.ReadFile(stub)
	if readErr != nil {
		t.Fatalf("read stub: %v", readErr)
	}
	stubSrc := string(stubData)
	for _, want := range []string{
		"package tools",
		"LoadTicketArgs",
		"LoadTicketResult",
		"func LoadTicket(ctx context.Context",
		"`json:\"input\"`",
		"`json:\"output\"`",
	} {
		if !strings.Contains(stubSrc, want) {
			t.Fatalf("tool stub missing %q in:\n%s", want, stubSrc)
		}
	}

	main, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatalf("read main.go: %v", readErr)
	}
	mainSrc := string(main)
	for _, want := range []string{
		`"demo/tools"`,
		`ovr.Tool("load_ticket", tools.LoadTicket`,
		"ovr.ReadOnly()",
		"Load ticket by id.",
	} {
		if !strings.Contains(mainSrc, want) {
			t.Fatalf("main.go missing %q after add tool:\n%s", want, mainSrc)
		}
	}
}

func TestAddToolRefusesIfStubExists(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tools", "load_ticket.go"), []byte("package tools\n"), 0o644); err != nil {
		t.Fatalf("write existing stub: %v", err)
	}
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "tool",
		"--name", "load_ticket",
		"--dir", dir,
	})
	if !errors.Is(err, ErrAdd) {
		t.Fatalf("Run() error = %v, want ErrAdd", err)
	}
}

func TestAddToolHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"add", "tool", "--help"}); err != nil {
		t.Fatalf("Run(add tool --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier add tool") {
		t.Fatalf("add tool help missing usage; got:\n%s", out.String())
	}
}
