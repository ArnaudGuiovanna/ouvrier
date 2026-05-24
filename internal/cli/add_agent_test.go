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

const fixtureMainGo = `package main

import (
	"log"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

type reply struct {
	Status string ` + "`json:\"status\"`" + `
}

func main() {
	if err := ovr.Run(":8080",
		ovr.From("POST /tickets"),
		ovr.Pipe("Handle the request.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
		),
		ovr.Reply(ovr.JSON[reply]()),
	); err != nil {
		log.Fatal(err)
	}
}
`

func writeAddFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(fixtureMainGo), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
}

func TestAddAgentRequiresName(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "agent", "--model", "anthropic/claude-sonnet-4-6"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddAgentRequiresValidModel(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "agent", "--name", "triage", "--model", "bogus"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddAgentRefusesWithoutPipYAML(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "agent",
		"--name", "triage",
		"--model", "anthropic/claude-sonnet-4-6",
		"--dir", dir,
	})
	if !errors.Is(err, ErrAdd) {
		t.Fatalf("Run() error = %v, want ErrAdd", err)
	}
	if !strings.Contains(err.Error(), "pip.yaml") {
		t.Fatalf("Run() error = %v, want pip.yaml mention", err)
	}
}

func TestAddAgentInsertsAfterExistingPipe(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "agent",
		"--name", "router",
		"--model", "anthropic/claude-sonnet-4-6",
		"--goal", "Route the ticket.",
		"--dir", dir,
	})
	if err != nil {
		t.Fatalf("Run() error = %v\nstderr=%s", err, errOut.String())
	}

	data, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatalf("read main.go: %v", readErr)
	}
	src := string(data)

	if strings.Count(src, "ovr.Pipe(") != 2 {
		t.Fatalf("expected 2 ovr.Pipe blocks; got:\n%s", src)
	}
	if !strings.Contains(src, `ovr.Pipe("Route the ticket."`) {
		t.Fatalf("new Pipe missing goal; got:\n%s", src)
	}
	if !strings.Contains(src, `ovr.Model("anthropic/claude-sonnet-4-6")`) {
		t.Fatalf("new Pipe missing model; got:\n%s", src)
	}
	// Sanity check: the file must still be valid Go (writeMainGo runs gofmt).
	if !strings.HasPrefix(src, "package main") {
		t.Fatalf("main.go corrupted; got:\n%s", src)
	}
}

func TestAddAgentRefusesWithoutAnchors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	// main.go without ovr.Pipe and without ovr.Reply/Push/Sink anchors.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "agent",
		"--name", "triage",
		"--model", "anthropic/claude-sonnet-4-6",
		"--dir", dir,
	})
	if !errors.Is(err, ErrMainEdit) {
		t.Fatalf("Run() error = %v, want ErrMainEdit", err)
	}
}

func TestAddSubcommandHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"add", "--help"}); err != nil {
		t.Fatalf("Run(add --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier add") {
		t.Fatalf("add help missing usage; got:\n%s", out.String())
	}
}

func TestAddUnknownSubcommandRejected(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "widget"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run(add widget) error = %v, want ErrUsage", err)
	}
}
