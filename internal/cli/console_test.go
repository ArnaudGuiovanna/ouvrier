package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestConsoleHelp(t *testing.T) {
	var out bytes.Buffer
	app := New("test", WithStreams(nil, &out, &out))
	if err := app.run(context.Background(), []string{"console", "--help"}); err != nil {
		t.Fatalf("console --help: %v", err)
	}
	if !strings.Contains(out.String(), "ouvrier console") {
		t.Fatalf("help missing banner: %s", out.String())
	}
}

func TestConsoleRejectsUnknownArg(t *testing.T) {
	var out bytes.Buffer
	app := New("test", WithStreams(nil, &out, &out))
	err := app.run(context.Background(), []string{"console", "--bogus"})
	if err == nil || !strings.Contains(err.Error(), "does not accept argument") {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestParseConsoleFlags(t *testing.T) {
	cfg, err := parseConsoleFlags([]string{"--addr", "127.0.0.1:9000", "--fleet=/tmp/f.json", "--no-open", "--token", "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:9000" || cfg.fleet != "/tmp/f.json" || !cfg.noOpen || cfg.token != "abc" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
}

// TestConsoleRefusesNonLoopback confirms the loopback rule is enforced at the
// CLI boundary (no insecure opt-in set).
func TestConsoleRefusesNonLoopback(t *testing.T) {
	t.Setenv("OUVRIER_CONSOLE_INSECURE", "")
	t.Setenv("OUVRIER_CONSOLE_ADDR", "")
	var out bytes.Buffer
	app := New("test", WithStreams(nil, &out, &out))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := app.run(ctx, []string{"console", "--addr", "0.0.0.0:7333", "--no-open"})
	if err == nil || !strings.Contains(err.Error(), "refusing to bind") {
		t.Fatalf("expected non-loopback refusal, got %v", err)
	}
}
