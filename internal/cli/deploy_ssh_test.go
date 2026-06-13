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

// The deploy engine itself (build, upload, systemd, health gate, rollback)
// lives in internal/deploy and is tested there; this file covers the CLI
// surface: flag parsing, routing, and help.

func TestParseDeploySSHFlagsRequiresHost(t *testing.T) {
	if _, err := parseDeploySSHFlags(nil); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseDeploySSHFlags() error = %v, want ErrUsage", err)
	}
}

func TestParseDeploySSHFlagsAcceptsAllOptions(t *testing.T) {
	cfg, err := parseDeploySSHFlags([]string{
		"--host", "ops@server",
		"--user", "deploy",
		"--port", "2222",
		"--path", "/srv/app",
		"--service", "demo.service",
		"--health-url", "/health",
		"--admin-token", "shh",
		"--dir", "/tmp/proj",
		"--identity", "/keys/ci_ed25519",
	})
	if err != nil {
		t.Fatalf("parseDeploySSHFlags() error = %v", err)
	}
	want := sshConfig{
		Host: "ops@server", User: "deploy", Port: 2222,
		Path: "/srv/app", Service: "demo.service",
		HealthURL: "/health", AdminToken: "shh", Dir: "/tmp/proj",
		Identity: "/keys/ci_ed25519",
	}
	if cfg != want {
		t.Fatalf("parseDeploySSHFlags() = %+v, want %+v", cfg, want)
	}
}

func TestParseDeploySSHFlagsRejectsBadPort(t *testing.T) {
	for _, bad := range []string{"abc", "-5", "0", "99999"} {
		if _, err := parseDeploySSHFlags([]string{"--host", "h", "--port", bad}); !errors.Is(err, ErrUsage) {
			t.Fatalf("--port %q error = %v, want ErrUsage", bad, err)
		}
	}
}

func TestParseDeploySSHFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseDeploySSHFlags([]string{"--host", "h", "--bogus"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseDeploySSHFlags() error = %v, want ErrUsage", err)
	}
}

func TestRunDeploySSHHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"deploy", "ssh", "--help"}); err != nil {
		t.Fatalf("Run(deploy ssh --help) error = %v", err)
	}
	for _, want := range []string{"Usage: ouvrier deploy ssh", "--host", "--admin-token", "--health-url"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deploy ssh help missing %q in:\n%s", want, out.String())
		}
	}
}

func TestDeployRouterRejectsUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "bogus"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("deploy bogus error = %v, want ErrUsage", err)
	}
}

func TestDeployRouterShowsHelpWithoutSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("deploy (no args) error = %v, want ErrUsage", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier deploy") {
		t.Fatalf("deploy without args did not print help: %s", out.String())
	}
}

func TestDeploySSHRunCommandPropagatesUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "ssh"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("deploy ssh (no host) error = %v, want ErrUsage", err)
	}
}

func TestParseDeploySSHFlagsUnitSandbox(t *testing.T) {
	for _, args := range [][]string{
		{"--host", "h", "--unit-sandbox", "off"},
		{"--host", "h", "--unit-sandbox=off"},
	} {
		cfg, err := parseDeploySSHFlags(args)
		if err != nil {
			t.Fatalf("parseDeploySSHFlags(%v) error = %v", args, err)
		}
		if cfg.UnitSandbox != "off" {
			t.Fatalf("UnitSandbox = %q, want off", cfg.UnitSandbox)
		}
	}
	cfg, err := parseDeploySSHFlags([]string{"--host", "h", "--unit-sandbox", "on"})
	if err != nil || cfg.UnitSandbox != "on" {
		t.Fatalf("parseDeploySSHFlags(--unit-sandbox on) = %+v, %v", cfg, err)
	}
	for _, bad := range []string{"maybe", "", "1"} {
		if _, err := parseDeploySSHFlags([]string{"--host", "h", "--unit-sandbox", bad}); !errors.Is(err, ErrUsage) {
			t.Fatalf("--unit-sandbox %q error = %v, want ErrUsage", bad, err)
		}
	}
}

// --print-sudoers is a local render: it needs no --host and must not deploy.
func TestParseDeploySSHFlagsPrintSudoersNeedsNoHost(t *testing.T) {
	cfg, err := parseDeploySSHFlags([]string{"--print-sudoers"})
	if err != nil {
		t.Fatalf("parseDeploySSHFlags(--print-sudoers) error = %v", err)
	}
	if !cfg.PrintSudoers {
		t.Fatal("PrintSudoers not set")
	}
}

func TestRunDeploySSHPrintSudoers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"deploy", "ssh", "--print-sudoers", "--dir", dir, "--user", "ci",
	})
	if err != nil {
		t.Fatalf("deploy ssh --print-sudoers error = %v", err)
	}
	for _, want := range []string{
		"ci ALL=(root) NOPASSWD: /usr/bin/systemctl restart ouvrier-demo.service",
		"/opt/ouvrier/demo",
		"/usr/sbin/useradd --system",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("sudoers output missing %q:\n%s", want, out.String())
		}
	}
}

// --print-sudoers without a pip.yaml is a deploy error naming the file, and
// the user falls back to the user@ part of --host when --user is absent.
func TestRunDeploySSHPrintSudoersFallbacks(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "ssh", "--print-sudoers", "--dir", t.TempDir()})
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "pip.yaml") {
		t.Fatalf("missing pip.yaml error = %v, want ErrDeploy naming pip.yaml", err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	out.Reset()
	if err := app.Run(context.Background(), []string{
		"deploy", "ssh", "--print-sudoers", "--dir", dir, "--host", "ops@server",
	}); err != nil {
		t.Fatalf("deploy ssh --print-sudoers --host error = %v", err)
	}
	if !strings.Contains(out.String(), "ops ALL=(root) NOPASSWD:") {
		t.Fatalf("sudoers output should use the user@ part of --host:\n%s", out.String())
	}
}
