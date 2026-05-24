package cli

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

// TestAddCommandsKeepScaffoldCompiling exercises the full flow: scaffold a
// new project, append an agent, a tool, and a skill, then run `go build` to
// prove the resulting source still compiles.
func TestAddCommandsKeepScaffoldCompiling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scaffold+add integration build in short mode")
	}
	root := repoRoot(t)
	parent := t.TempDir()

	project, err := scaffold.Generate(context.Background(), scaffold.Config{
		Name:         "demo",
		Trigger:      "POST /tickets",
		Model:        "anthropic/claude-sonnet-4-6",
		Dir:          parent,
		FrameworkDir: root,
	})
	if err != nil {
		t.Fatalf("scaffold.Generate() error = %v", err)
	}

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))

	if err := app.Run(context.Background(), []string{
		"add", "agent",
		"--name", "router",
		"--model", "anthropic/claude-sonnet-4-6",
		"--goal", "Route the ticket.",
		"--dir", project.Dir,
	}); err != nil {
		t.Fatalf("add agent: %v\nstderr=%s", err, errOut.String())
	}

	if err := app.Run(context.Background(), []string{
		"add", "tool",
		"--name", "load_ticket",
		"--describe", "Load ticket by id.",
		"--readonly",
		"--dir", project.Dir,
	}); err != nil {
		t.Fatalf("add tool: %v\nstderr=%s", err, errOut.String())
	}

	if err := app.Run(context.Background(), []string{
		"add", "skill",
		"--name", "moodle-fsrs",
		"--description", "Schedule FSRS reviews.",
		"--dir", project.Dir,
	}); err != nil {
		t.Fatalf("add skill: %v\nstderr=%s", err, errOut.String())
	}

	cmd := exec.Command("go", "build", "-buildvcs=false", "./...")
	cmd.Dir = project.Dir
	if buildOutput, buildErr := cmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("scaffolded project no longer compiles after add commands: %v\n%s", buildErr, string(buildOutput))
	}

	// Sanity assertion: stdout reports the three add messages.
	if !strings.Contains(out.String(), "added agent") ||
		!strings.Contains(out.String(), "added tool") ||
		!strings.Contains(out.String(), "added skill") {
		t.Fatalf("stdout missing add messages:\n%s", out.String())
	}
}
