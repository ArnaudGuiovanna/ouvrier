package codex

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type fakeRunner struct{ jsonl string }

func (f fakeRunner) LookPath(string) (string, error) { return "/usr/bin/codex", nil }
func (f fakeRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null; printf '%s' "+shellQuote(f.jsonl))
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestCodexProviderCompleteText(t *testing.T) {
	jsonl := `{"type":"item.completed","item":{"text":"Hello from Codex"}}` + "\n" +
		`{"type":"turn.completed"}` + "\n"
	p := &Provider{Runner: fakeRunner{jsonl: jsonl}, Model: "gpt-5-codex"}
	if p.Name() != "codex" {
		t.Fatalf("name = %q", p.Name())
	}
	resp, err := p.Complete(context.Background(), provider.Request{
		Model:    "codex/gpt-5-codex",
		System:   "you are a worker factory",
		Messages: []provider.Message{provider.UserText("build a worker")},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !strings.Contains(resp.Text, "Hello from Codex") {
		t.Fatalf("text = %q", resp.Text)
	}
	if resp.StopReason != provider.StopEndTurn {
		t.Fatalf("stop = %q", resp.StopReason)
	}
}

func TestCodexProviderStreamsDeltas(t *testing.T) {
	jsonl := `{"type":"item.completed","item":{"text":"chunk one"}}` + "\n"
	p := &Provider{Runner: fakeRunner{jsonl: jsonl}, Model: "gpt-5-codex"}
	var got string
	_, err := p.CompleteStream(context.Background(), provider.Request{Messages: []provider.Message{provider.UserText("hi")}}, func(d provider.Delta) { got += d.Text })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(got, "chunk one") {
		t.Fatalf("delta not streamed: %q", got)
	}
}
