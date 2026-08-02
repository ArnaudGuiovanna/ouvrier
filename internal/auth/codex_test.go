package auth

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	statusOut string
	deviceOut string
	block     bool
}

func (f fakeRunner) LookPath(string) (string, error) { return "/usr/bin/codex", nil }
func (f fakeRunner) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if f.block {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}
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

func TestProbeRejectsTruncatedOutput(t *testing.T) {
	out := "Logged in using ChatGPT\n" + strings.Repeat("x", maxCodexAuthOutputBytes+1)
	b := &Codex{Runner: fakeRunner{statusOut: out}}
	if st, account := b.Probe(context.Background()); st != StateUnauthed || account != "" {
		t.Fatalf("truncated probe = %v, %q; want fail-closed unauthenticated state", st, account)
	}
}

func TestProbeHonorsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	b := &Codex{Runner: fakeRunner{block: true}}
	if st, _ := b.Probe(ctx); st != StateUnauthed {
		t.Fatalf("timed-out probe state = %v", st)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("probe exceeded caller deadline: %s", elapsed)
	}
}

func TestDeviceLoginBoundsRawOutput(t *testing.T) {
	prefix := "Open https://auth.openai.com/codex/device and enter ABCD-EFGH\n"
	b := &Codex{Runner: fakeRunner{deviceOut: prefix + strings.Repeat("x", maxCodexAuthOutputBytes+1)}}
	ev, err := b.DeviceLogin(context.Background())
	if !errors.Is(err, ErrCodexAuthOutputLimit) {
		t.Fatalf("DeviceLogin() error = %v, want output-limit error", err)
	}
	if len(ev.Raw) != maxCodexAuthOutputBytes {
		t.Fatalf("bounded raw output = %d bytes, want %d", len(ev.Raw), maxCodexAuthOutputBytes)
	}
	if ev.URL == "" || ev.Code != "ABCD-EFGH" {
		t.Fatalf("bounded device event lost parsed prefix: %+v", ev)
	}
}

func TestDeviceLoginHonorsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	b := &Codex{Runner: fakeRunner{block: true}}
	_, err := b.DeviceLogin(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeviceLogin() error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("device login exceeded caller deadline: %s", elapsed)
	}
}

func TestBoundedAuthCaptureReportsFullWritesWithoutGrowing(t *testing.T) {
	capture := newBoundedAuthCapture(8)
	data := []byte("0123456789abcdef")
	written, err := capture.Write(data)
	if err != nil || written != len(data) {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if got := capture.String(); got != "01234567" || !capture.Truncated() {
		t.Fatalf("bounded capture = %q, truncated=%v", got, capture.Truncated())
	}
}
