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

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// Real ed25519 key generated with ssh-keygen; the fingerprint is the exact
// `ssh-keygen -lf` output for it.
const (
	testEd25519Key = "AAAAC3NzaC1lZDI1NTE5AAAAIOC1fBGmprTRgKWy2+g5QqqOQ6X95acZ6pbByKW5b8yK"
	testEd25519FP  = "SHA256:f/+IMT34E8qsxk2X/ZKlziiV1CdRTKI1BGin66IkrsQ"
)

// fakeKeyscan returns canned ssh-keyscan output and records the host it was
// asked to scan.
func fakeKeyscan(output string) (deploy.KeyscanRunner, *string) {
	scanned := new(string)
	return func(_ context.Context, host string, _ int) (string, error) {
		*scanned = host
		return output, nil
	}, scanned
}

func keyscanFixture(host string) string {
	return "# " + host + ":22 SSH-2.0-OpenSSH_9.6\n" +
		host + " ssh-rsa AAAAB3rsa\n" +
		host + " ssh-ed25519 " + testEd25519Key + "\n"
}

func newTrustApp(t *testing.T, in io.Reader, keyscanOut string) (*App, *bytes.Buffer, string, *string) {
	t.Helper()
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(in, &out, &errOut))
	runner, scanned := fakeKeyscan(keyscanOut)
	app.keyscan = runner
	return app, &out, dir, scanned
}

func TestServerTrustWithFingerprintPinsNonInteractively(t *testing.T) {
	app, out, dir, scanned := newTrustApp(t, nil, keyscanFixture("h"))
	err := app.Run(context.Background(), []string{
		"server", "trust", "h", "--fingerprint", testEd25519FP, "--dir", dir,
	})
	if err != nil {
		t.Fatalf("server trust error = %v", err)
	}
	if *scanned != "h" {
		t.Fatalf("keyscan host = %q, want h", *scanned)
	}
	data, readErr := os.ReadFile(filepath.Join(dir, "ouvrier.known_hosts"))
	if readErr != nil {
		t.Fatalf("read ouvrier.known_hosts: %v", readErr)
	}
	// All scanned key types are pinned, not just the fingerprinted one.
	for _, want := range []string{
		"h ssh-rsa AAAAB3rsa",
		"h ssh-ed25519 " + testEd25519Key,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("ouvrier.known_hosts missing %q:\n%s", want, string(data))
		}
	}
	for _, want := range []string{testEd25519FP, "pinned 2 host key(s)", "commit"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestServerTrustFingerprintWithoutPrefixAccepted(t *testing.T) {
	app, _, dir, _ := newTrustApp(t, nil, keyscanFixture("h"))
	err := app.Run(context.Background(), []string{
		"server", "trust", "h",
		"--fingerprint", strings.TrimPrefix(testEd25519FP, "SHA256:"),
		"--dir", dir,
	})
	if err != nil {
		t.Fatalf("server trust error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ouvrier.known_hosts")); statErr != nil {
		t.Fatalf("ouvrier.known_hosts not written: %v", statErr)
	}
}

func TestServerTrustFingerprintMismatchAbortsWithoutWriting(t *testing.T) {
	app, _, dir, _ := newTrustApp(t, nil, keyscanFixture("h"))
	err := app.Run(context.Background(), []string{
		"server", "trust", "h", "--fingerprint", "SHA256:WRONGwrongWRONGwrongWRONGwrongWRONGwrongWRO", "--dir", dir,
	})
	if !errors.Is(err, ErrTrust) {
		t.Fatalf("server trust error = %v, want ErrTrust", err)
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("server trust error = %v, want fingerprint mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ouvrier.known_hosts")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("mismatch must not write ouvrier.known_hosts; stat = %v", statErr)
	}
}

func TestServerTrustInteractiveConfirm(t *testing.T) {
	app, out, dir, _ := newTrustApp(t, strings.NewReader("y\n"), keyscanFixture("h"))
	if err := app.Run(context.Background(), []string{"server", "trust", "h", "--dir", dir}); err != nil {
		t.Fatalf("server trust error = %v", err)
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("missing confirmation prompt:\n%s", out.String())
	}
	if !strings.Contains(out.String(), testEd25519FP) {
		t.Fatalf("fingerprint not displayed before confirmation:\n%s", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ouvrier.known_hosts")); statErr != nil {
		t.Fatalf("ouvrier.known_hosts not written after confirm: %v", statErr)
	}
}

func TestServerTrustInteractiveDeclineWritesNothing(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", ""} {
		app, _, dir, _ := newTrustApp(t, strings.NewReader(answer), keyscanFixture("h"))
		err := app.Run(context.Background(), []string{"server", "trust", "h", "--dir", dir})
		if !errors.Is(err, ErrTrust) {
			t.Fatalf("answer %q: error = %v, want ErrTrust", answer, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "ouvrier.known_hosts")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("answer %q: decline must not write; stat = %v", answer, statErr)
		}
	}
}

func TestServerTrustSameKeyIsFriendlyNoOp(t *testing.T) {
	app, out, dir, _ := newTrustApp(t, nil, keyscanFixture("h"))
	args := []string{"server", "trust", "h", "--fingerprint", testEd25519FP, "--dir", dir}
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("first trust error = %v", err)
	}
	out.Reset()
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("second trust error = %v", err)
	}
	if !strings.Contains(out.String(), "already trusted") {
		t.Fatalf("expected friendly no-op message, got:\n%s", out.String())
	}
}

func TestServerTrustChangedKeyRequiresRotate(t *testing.T) {
	dir := t.TempDir()
	seed := "h ssh-ed25519 AAAAC3oldOLDold\n"
	if err := os.WriteFile(filepath.Join(dir, "ouvrier.known_hosts"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	runner, _ := fakeKeyscan(keyscanFixture("h"))
	app.keyscan = runner

	err := app.Run(context.Background(), []string{"server", "trust", "h", "--fingerprint", testEd25519FP, "--dir", dir})
	if !errors.Is(err, ErrTrust) {
		t.Fatalf("server trust error = %v, want ErrTrust", err)
	}
	if !strings.Contains(err.Error(), "ouvrier server trust --rotate h") {
		t.Fatalf("server trust error = %v, want --rotate hint", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "ouvrier.known_hosts"))
	if string(data) != seed {
		t.Fatalf("changed-key refusal modified the file:\n%s", string(data))
	}

	// --rotate replaces the stale entry.
	err = app.Run(context.Background(), []string{"server", "trust", "h", "--rotate", "--fingerprint", testEd25519FP, "--dir", dir})
	if err != nil {
		t.Fatalf("server trust --rotate error = %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "ouvrier.known_hosts"))
	if strings.Contains(string(data), "AAAAC3oldOLDold") {
		t.Fatalf("--rotate kept the old key:\n%s", string(data))
	}
	if !strings.Contains(string(data), testEd25519Key) {
		t.Fatalf("--rotate did not pin the new key:\n%s", string(data))
	}
	if !strings.Contains(out.String(), "rotated") {
		t.Fatalf("missing rotated message:\n%s", out.String())
	}
}

func TestServerTrustStripsUserAndPinsPortQualifiedHost(t *testing.T) {
	app, _, dir, scanned := newTrustApp(t, nil, "[h]:2222 ssh-ed25519 "+testEd25519Key+"\n")
	err := app.Run(context.Background(), []string{
		"server", "trust", "ops@h", "--port", "2222", "--fingerprint", testEd25519FP, "--dir", dir,
	})
	if err != nil {
		t.Fatalf("server trust error = %v", err)
	}
	if *scanned != "h" {
		t.Fatalf("keyscan host = %q, want user@ stripped", *scanned)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "ouvrier.known_hosts"))
	if !strings.HasPrefix(string(data), "[h]:2222 ssh-ed25519 ") {
		t.Fatalf("ouvrier.known_hosts = %q, want [h]:2222 entry", string(data))
	}
}

func TestServerTrustEmptyScanFails(t *testing.T) {
	app, _, dir, _ := newTrustApp(t, nil, "# only comments\n")
	err := app.Run(context.Background(), []string{"server", "trust", "h", "--fingerprint", testEd25519FP, "--dir", dir})
	if !errors.Is(err, ErrTrust) {
		t.Fatalf("server trust error = %v, want ErrTrust", err)
	}
	if !strings.Contains(err.Error(), "no host keys") {
		t.Fatalf("server trust error = %v, want no-host-keys message", err)
	}
}

func TestServerTrustNoInputRequiresFingerprint(t *testing.T) {
	app, _, dir, _ := newTrustApp(t, nil, keyscanFixture("h"))
	err := app.Run(context.Background(), []string{"server", "trust", "h", "--dir", dir})
	if !errors.Is(err, ErrTrust) {
		t.Fatalf("server trust error = %v, want ErrTrust", err)
	}
	if !strings.Contains(err.Error(), "--fingerprint") {
		t.Fatalf("server trust error = %v, want --fingerprint hint", err)
	}
}

func TestParseServerTrustFlags(t *testing.T) {
	cfg, err := parseServerTrustFlags([]string{"h", "--port", "2222", "--fingerprint", "abc", "--rotate", "--dir", "/p"})
	if err != nil {
		t.Fatalf("parseServerTrustFlags() error = %v", err)
	}
	want := trustConfig{Host: "h", Port: 2222, Fingerprint: "abc", Rotate: true, Dir: "/p"}
	if cfg != want {
		t.Fatalf("parseServerTrustFlags() = %+v, want %+v", cfg, want)
	}

	if _, err := parseServerTrustFlags(nil); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseServerTrustFlags(nil) error = %v, want ErrUsage", err)
	}
	if _, err := parseServerTrustFlags([]string{"a", "b"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("two hosts error = %v, want ErrUsage", err)
	}
	if _, err := parseServerTrustFlags([]string{"h", "--bogus"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("unknown flag error = %v, want ErrUsage", err)
	}
}

func TestServerRouter(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"server"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("server (no args) error = %v, want ErrUsage", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier server") {
		t.Fatalf("server without args did not print help: %s", out.String())
	}
	if err := app.Run(context.Background(), []string{"server", "bogus"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("server bogus error = %v, want ErrUsage", err)
	}
}

func TestServerTrustHelpDocumentsFingerprintChoice(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"server", "trust", "--help"}); err != nil {
		t.Fatalf("server trust --help error = %v", err)
	}
	for _, want := range []string{
		"Usage: ouvrier server trust <host>",
		"--fingerprint",
		"--rotate",
		"ed25519",
		"ouvrier.known_hosts",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("server trust help missing %q:\n%s", want, out.String())
		}
	}
}
