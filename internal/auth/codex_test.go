package auth

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

type fakeRunner struct {
	statusOut string
	deviceOut string
}

func (f fakeRunner) LookPath(string) (string, error) { return "/usr/bin/codex", nil }
func (f fakeRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	out := ""
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "login status"):
		out = f.statusOut
	case strings.Contains(joined, "--device-auth"):
		out = f.deviceOut
	}
	return exec.CommandContext(ctx, "sh", "-c", "printf '%s' "+shellQuote(out))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestProbeAuthed(t *testing.T) {
	b := &Codex{Runner: fakeRunner{statusOut: "Logged in using ChatGPT\n"}}
	st, _ := b.Probe(context.Background())
	if st != StateAuthed {
		t.Fatalf("state = %v, want authed", st)
	}
}

func TestProbeUnauthed(t *testing.T) {
	b := &Codex{Runner: fakeRunner{statusOut: "Not logged in\n"}}
	if st, _ := b.Probe(context.Background()); st != StateUnauthed {
		t.Fatalf("state = %v, want unauthed", st)
	}
}

func TestDeviceLoginParsesURLAndCode(t *testing.T) {
	out := "To sign in, open https://auth.openai.com/codex/device and enter code ABCD-EFGH\n"
	b := &Codex{Runner: fakeRunner{deviceOut: out}}
	ev, err := b.DeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	if ev.URL == "" || !strings.Contains(ev.URL, "auth.openai.com") {
		t.Fatalf("url not parsed: %q (raw=%q)", ev.URL, ev.Raw)
	}
	if ev.Code == "" {
		t.Fatalf("code not parsed (raw=%q)", ev.Raw)
	}
	if ev.Raw == "" {
		t.Fatal("raw output must always be captured")
	}
}
