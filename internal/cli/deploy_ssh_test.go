package cli

import (
	"bytes"
	"context"
	"errors"
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
	})
	if err != nil {
		t.Fatalf("parseDeploySSHFlags() error = %v", err)
	}
	want := sshConfig{
		Host: "ops@server", User: "deploy", Port: 2222,
		Path: "/srv/app", Service: "demo.service",
		HealthURL: "/health", AdminToken: "shh", Dir: "/tmp/proj",
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
