package codex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestProviderRejectsOversizedJSONLLine(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "oversized-line"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := p.Complete(ctx, provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err == nil || !strings.Contains(err.Error(), "stdout line exceeds") {
		t.Fatalf("Complete() error = %v, want explicit oversized-line failure", err)
	}
	if len(resp.Text) > maxCodexExecTextBytes {
		t.Fatalf("response text bytes = %d, want <= %d", len(resp.Text), maxCodexExecTextBytes)
	}
}

func TestProviderBoundsCumulativeAssistantText(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "cumulative-text"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streamed := 0

	resp, err := p.CompleteStream(ctx, provider.Request{Messages: []provider.Message{provider.UserText("test")}}, func(delta provider.Delta) {
		streamed += len(delta.Text)
	})
	if err == nil || !strings.Contains(err.Error(), "assistant text exceeds") {
		t.Fatalf("CompleteStream() error = %v, want explicit cumulative-text failure", err)
	}
	if len(resp.Text) > maxCodexExecTextBytes {
		t.Fatalf("response text bytes = %d, want <= %d", len(resp.Text), maxCodexExecTextBytes)
	}
	if streamed > maxCodexExecTextBytes {
		t.Fatalf("streamed text bytes = %d, want <= %d", streamed, maxCodexExecTextBytes)
	}
}

func TestProviderBoundsCumulativeStdout(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "cumulative-stdout"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := p.Complete(ctx, provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err == nil || !strings.Contains(err.Error(), "stdout exceeds") {
		t.Fatalf("Complete() error = %v, want explicit cumulative-stdout failure", err)
	}
}

func TestProviderBoundsStderrDiagnostics(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "oversized-stderr"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := p.Complete(ctx, provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err == nil {
		t.Fatal("Complete() error = nil, want process failure")
	}
	if !strings.Contains(err.Error(), "[stderr truncated]") {
		t.Fatalf("Complete() error does not report bounded stderr truncation: %v", err)
	}
	if len(err.Error()) > maxCodexExecStderrBytes+512 {
		t.Fatalf("Complete() error bytes = %d, want bounded diagnostic", len(err.Error()))
	}
}

func TestProviderRejectsMalformedJSONLAndReturnsBoundedPrefix(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "malformed-json"}, Bin: "codex-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()

	resp, err := p.Complete(ctx, provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err == nil || !strings.Contains(err.Error(), "invalid JSONL") {
		t.Fatalf("Complete() error = %v, want explicit malformed-JSONL failure", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Complete() elapsed = %s, malformed output did not cancel the process", elapsed)
	}
	if resp.Text != "accepted prefix" {
		t.Fatalf("response text = %q, want accepted bounded prefix", resp.Text)
	}
}

func TestProviderIgnoresValidUnknownJSONLEvent(t *testing.T) {
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "unknown-json"}, Bin: "codex-test"}
	resp, err := p.Complete(context.Background(), provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "" || resp.StopReason != provider.StopEndTurn {
		t.Fatalf("response = %+v, want empty successful response", resp)
	}
}

func TestProviderDoesNotTransmitArbitraryEnvironmentSecrets(t *testing.T) {
	const secret = "codex-exec-worker-secret"
	t.Setenv("OUVRIER_CODEX_EXEC_TEST_SECRET", secret)
	p := &Provider{Runner: codexExecAdversarialRunner{mode: "inspect-environment"}, Bin: "codex-test"}

	resp, err := p.Complete(context.Background(), provider.Request{Messages: []provider.Message{provider.UserText("test")}})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Text != "secret absent" || strings.Contains(resp.Text, secret) {
		t.Fatalf("Codex subprocess observed parent secret: %q", resp.Text)
	}
}

type codexExecAdversarialRunner struct{ mode string }

func (r codexExecAdversarialRunner) CommandContext(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexExecProviderHelperProcess$", "--", "codex-exec-helper="+r.mode)
}

func (codexExecAdversarialRunner) LookPath(string) (string, error) { return os.Args[0], nil }

func TestCodexExecProviderHelperProcess(t *testing.T) {
	mode := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "codex-exec-helper=") {
			mode = strings.TrimPrefix(arg, "codex-exec-helper=")
			break
		}
	}
	if mode == "" {
		return
	}

	switch mode {
	case "oversized-line":
		fmt.Fprintf(os.Stdout, `{"type":"item.completed","item":{"text":"%s"}}`, strings.Repeat("x", maxCodexExecLineBytes+1))
	case "cumulative-text":
		chunk := strings.Repeat("x", 64<<10)
		for range maxCodexExecTextBytes/len(chunk) + 2 {
			fmt.Fprintf(os.Stdout, `{"type":"item.completed","item":{"text":"%s"}}`+"\n", chunk)
		}
	case "cumulative-stdout":
		line := []byte(`{"type":"unknown","padding":"` + strings.Repeat("x", 64<<10) + `"}`)
		for range maxCodexExecStdoutBytes/(len(line)+1) + 2 {
			_, _ = os.Stdout.Write(line)
			_, _ = os.Stdout.Write([]byte{'\n'})
		}
	case "oversized-stderr":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), maxCodexExecStderrBytes+32<<10))
		os.Exit(23)
	case "malformed-json":
		fmt.Fprintln(os.Stdout, `{"type":"item.completed","item":{"text":"accepted prefix"}}`)
		fmt.Fprintln(os.Stdout, `{"type":`)
		for {
			time.Sleep(time.Hour)
		}
	case "unknown-json":
		fmt.Fprintln(os.Stdout, `{"type":"turn.started","metadata":{"valid":true}}`)
		os.Exit(0)
	case "inspect-environment":
		text := "secret absent"
		if secret, ok := os.LookupEnv("OUVRIER_CODEX_EXEC_TEST_SECRET"); ok {
			text = "secret leaked: " + secret
		}
		fmt.Fprintf(os.Stdout, `{"type":"item.completed","item":{"text":%q}}`+"\n", text)
		os.Exit(0)
	case "inherited-pipe-parent":
		child := exec.Command(os.Args[0], "-test.run=^TestCodexExecProviderHelperProcess$", "--", "codex-exec-helper=inherited-pipe-grandchild")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatalf("start inherited-pipe grandchild: %v", err)
		}
		fmt.Fprintf(os.Stdout, `{"type":"item.completed","item":{"text":"grandchild_pid=%d"}}`+"\n", child.Process.Pid)
		os.Exit(0)
	case "inherited-pipe-grandchild":
		for {
			time.Sleep(time.Hour)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}
