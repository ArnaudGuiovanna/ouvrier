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

func TestAddTriggerRequiresTrigger(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"add", "trigger", "--model", "anthropic/claude-sonnet-4-6"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("Run() error = %v, want ErrUsage", err)
	}
}

func TestAddTriggerAppendsCronPipeline(t *testing.T) {
	dir := t.TempDir()
	writeAddFixture(t, dir)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"add", "trigger",
		"--trigger", "cron @every 1h",
		"--model", "openai/gpt-4.1-mini",
		"--goal", "Summarize recent events.",
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
	for _, want := range []string{
		`ovr.From(ovr.Cron("@every 1h"))`,
		`ovr.Pipe("Summarize recent events."`,
		`ovr.Model("openai/gpt-4.1-mini")`,
		`ovr.Sink(ovr.Log())`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("main.go missing %q after add trigger:\n%s", want, src)
		}
	}
	if strings.Count(src, "ovr.From(") != 2 {
		t.Fatalf("expected 2 ovr.From blocks; got:\n%s", src)
	}
	if !strings.Contains(out.String(), `added trigger "cron @every 1h"`) {
		t.Fatalf("stdout missing add trigger message:\n%s", out.String())
	}
}

func TestAddTriggerHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"add", "trigger", "--help"}); err != nil {
		t.Fatalf("Run(add trigger --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier add trigger") {
		t.Fatalf("add trigger help missing usage; got:\n%s", out.String())
	}
}
