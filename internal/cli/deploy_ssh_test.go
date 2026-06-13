package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// The deploy engine itself (build, upload, systemd, health gate, rollback)
// lives in internal/deploy and is tested there; this file covers the CLI
// surface: flag parsing, env resolution from pip.yaml, prod confirmation,
// routing, and help.

func TestParseDeployEnvFlagsDefaults(t *testing.T) {
	cfg, err := parseDeployEnvFlags(nil)
	if err != nil {
		t.Fatalf("parseDeployEnvFlags() error = %v", err)
	}
	if cfg.Dir != "." || cfg.Keep != deploy.DefaultKeepReleases {
		t.Fatalf("defaults = %+v, want Dir=. Keep=%d", cfg, deploy.DefaultKeepReleases)
	}
}

func TestParseDeployEnvFlagsAcceptsAllOptions(t *testing.T) {
	cfg, err := parseDeployEnvFlags([]string{
		"--host", "ops@server",
		"--user", "deploy",
		"--port", "2222",
		"--path", "/srv/app",
		"--service", "demo.service",
		"--dir", "/tmp/proj",
		"--env-file", ".env.ci",
		"--identity", "/keys/ci_ed25519",
		"--target", "linux/arm64",
		"--keep", "3",
		"--yes",
		"--allow-shared-admin",
		"--unit-sandbox", "off",
	})
	if err != nil {
		t.Fatalf("parseDeployEnvFlags() error = %v", err)
	}
	want := deployEnvConfig{
		Host: "ops@server", User: "deploy", Port: 2222,
		Path: "/srv/app", Service: "demo.service", Dir: "/tmp/proj",
		EnvFile: ".env.ci", Identity: "/keys/ci_ed25519",
		Target: "linux/arm64", Keep: 3,
		Yes: true, AllowSharedAdmin: true, UnitSandbox: "off",
	}
	if cfg != want {
		t.Fatalf("parseDeployEnvFlags() = %+v, want %+v", cfg, want)
	}

	// --flag=value forms parse identically.
	cfg2, err := parseDeployEnvFlags([]string{"--host=ops@server", "--keep=3", "--target=linux/arm64"})
	if err != nil {
		t.Fatalf("parseDeployEnvFlags(inline) error = %v", err)
	}
	if cfg2.Host != "ops@server" || cfg2.Keep != 3 || cfg2.Target != "linux/arm64" {
		t.Fatalf("inline forms = %+v", cfg2)
	}
}

func TestParseDeployEnvFlagsRejectsBadValues(t *testing.T) {
	cases := [][]string{
		{"--port", "abc"},
		{"--port", "0"},
		{"--port", "99999"},
		{"--keep", "0"},
		{"--keep", "x"},
		{"--target", "linux"},
		{"--target", "linux/"},
		{"--unit-sandbox", "maybe"},
		{"--bogus"},
		{"--host"},
	}
	for _, args := range cases {
		if _, err := parseDeployEnvFlags(args); !errors.Is(err, ErrUsage) {
			t.Fatalf("parseDeployEnvFlags(%v) error = %v, want ErrUsage", args, err)
		}
	}
}

func TestDeploySSHRequiresHost(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "ssh"})
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("deploy ssh (no host) error = %v, want ErrUsage naming --host", err)
	}
}

func TestDeployRouterRejectsBareFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "--host", "h"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("deploy --host error = %v, want ErrUsage", err)
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

func TestRunDeploySSHHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"deploy", "ssh", "--help"}); err != nil {
		t.Fatalf("Run(deploy ssh --help) error = %v", err)
	}
	for _, want := range []string{
		"Usage: ouvrier deploy <env>",
		"--host", "--env-file", "--identity", "--target", "--keep",
		"--yes", "--allow-shared-admin", "--print-sudoers",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deploy ssh help missing %q in:\n%s", want, out.String())
		}
	}
}

// writeDeployProject writes a pip.yaml with deploy environments for the CLI
// resolution tests. No host is pinned, so any deploy that reaches the engine
// fails at the trust gate — proving how far the CLI got without a network.
func writeDeployProject(t *testing.T, pipYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte(pipYAML), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	return dir
}

const deployProjectYAML = `name: demo
version: 0.1.0

deploy:
  staging:
    hosts: [deploy@stg-1.example.com]
    port: 2222
    path: /srv/workers/demo
    service: demo-worker
    identity: ~/.ssh/ci_ed25519
    sandbox: off
  prod:
    hosts:
      - deploy@prod-1.example.com
      - deploy@prod-2.example.com
`

func TestDeployEnvUnknownEnvironmentListsKnown(t *testing.T) {
	dir := writeDeployProject(t, deployProjectYAML)
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "nosuch", "--dir", dir})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("deploy nosuch error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"deploy.nosuch", "staging", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want mention of %q", err, want)
		}
	}
}

func TestDeployEnvRequiresPipYAML(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "staging", "--dir", t.TempDir()})
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "pip.yaml") {
		t.Fatalf("deploy without pip.yaml error = %v, want ErrDeploy naming pip.yaml", err)
	}
}

// resolveDeployEnvOpts merges flags over pip.yaml deploy.<env> values:
// identity, port, path, service, sandbox — flags win.
func TestResolveDeployEnvOptsEnvDefaultsAndPrecedence(t *testing.T) {
	summary := parsePipYAML(deployProjectYAML)

	// Defaults from pip.yaml deploy.staging.
	opts, err := resolveDeployEnvOpts(deployEnvConfig{Dir: ".", Keep: 5}, "staging", summary)
	if err != nil {
		t.Fatalf("resolveDeployEnvOpts() error = %v", err)
	}
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "deploy@stg-1.example.com" {
		t.Fatalf("Hosts = %v", opts.Hosts)
	}
	if opts.Port != 2222 || opts.Path != "/srv/workers/demo" || opts.Service != "demo-worker" {
		t.Fatalf("env defaults not applied: %+v", opts)
	}
	if opts.Identity != "~/.ssh/ci_ed25519" {
		t.Fatalf("Identity = %q, want pip.yaml identity (carry-over c)", opts.Identity)
	}
	if opts.EnvSandbox != "off" {
		t.Fatalf("EnvSandbox = %q, want off", opts.EnvSandbox)
	}
	if opts.EnvName != "staging" {
		t.Fatalf("EnvName = %q", opts.EnvName)
	}

	// Flags win over pip.yaml values; --host narrows the host list.
	cfg := deployEnvConfig{
		Dir: ".", Keep: 5,
		Host: "ops@override", Port: 22, Path: "/opt/x", Service: "svc",
		Identity: "/keys/other",
	}
	opts, err = resolveDeployEnvOpts(cfg, "staging", summary)
	if err != nil {
		t.Fatalf("resolveDeployEnvOpts() error = %v", err)
	}
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "ops@override" {
		t.Fatalf("--host must narrow the env hosts: %v", opts.Hosts)
	}
	if opts.Identity != "/keys/other" {
		t.Fatalf("--identity must win over pip.yaml: %q", opts.Identity)
	}
	if opts.Port != 22 || opts.Path != "/opt/x" || opts.Service != "svc" {
		t.Fatalf("flags must win: %+v", opts)
	}

	// The ssh alias (no env name) requires no registry and keeps multi-host off.
	opts, err = resolveDeployEnvOpts(deployEnvConfig{Dir: ".", Keep: 5, Host: "ops@h"}, "", summary)
	if err != nil {
		t.Fatalf("resolveDeployEnvOpts(ssh alias) error = %v", err)
	}
	if len(opts.Hosts) != 1 || opts.Hosts[0] != "ops@h" || opts.EnvName != "" {
		t.Fatalf("ssh alias opts = %+v", opts)
	}
}

// OUVRIER_DEPLOY_ENV_FILE is the documented --env-file fallback; the flag wins.
func TestResolveDeployEnvOptsHonorsDeployEnvFileVar(t *testing.T) {
	summary := parsePipYAML(deployProjectYAML)
	t.Setenv(envnames.DeployEnvFile, "/secrets/ci.env")

	opts, err := resolveDeployEnvOpts(deployEnvConfig{Dir: ".", Keep: 5, Host: "h"}, "", summary)
	if err != nil {
		t.Fatalf("resolveDeployEnvOpts() error = %v", err)
	}
	if opts.EnvFile != "/secrets/ci.env" {
		t.Fatalf("EnvFile = %q, want OUVRIER_DEPLOY_ENV_FILE value", opts.EnvFile)
	}

	opts, err = resolveDeployEnvOpts(deployEnvConfig{Dir: ".", Keep: 5, Host: "h", EnvFile: ".env.flag"}, "", summary)
	if err != nil {
		t.Fatalf("resolveDeployEnvOpts() error = %v", err)
	}
	if opts.EnvFile != ".env.flag" {
		t.Fatalf("EnvFile = %q, want the --env-file flag to win", opts.EnvFile)
	}
}

// Acceptance: deploying to prod/production demands --yes or an interactive
// confirmation; declining aborts before any work.
func TestDeployProdRequiresConfirmation(t *testing.T) {
	dir := writeDeployProject(t, deployProjectYAML)

	t.Run("non-interactive without --yes", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(nil, &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "prod", "--dir", dir})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("error = %v, want confirmation requirement naming --yes", err)
		}
	})

	t.Run("interactive decline", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(strings.NewReader("n\n"), &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "prod", "--dir", dir})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("error = %v, want abort after decline", err)
		}
		if !strings.Contains(out.String(), "Deploy to prod") || !strings.Contains(out.String(), "[y/N]") {
			t.Fatalf("missing confirmation prompt:\n%s", out.String())
		}
	})

	t.Run("interactive accept proceeds", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(strings.NewReader("y\n"), &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "prod", "--dir", dir})
		// No host is pinned: an accepted confirm reaches the engine's trust
		// gate, proving the confirmation passed.
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust") {
			t.Fatalf("error = %v, want trust-gate failure after accepted confirm", err)
		}
	})

	t.Run("--yes skips the prompt", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(nil, &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "prod", "--dir", dir, "--yes"})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust") {
			t.Fatalf("error = %v, want trust-gate failure (confirmation skipped)", err)
		}
		if strings.Contains(out.String(), "[y/N]") {
			t.Fatalf("--yes must not prompt:\n%s", out.String())
		}
	})

	t.Run("staging never prompts", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(nil, &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "staging", "--dir", dir})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust") {
			t.Fatalf("error = %v, want trust-gate failure without any prompt", err)
		}
	})
}

// --print-sudoers is a local render: it needs no --host and must not deploy.
func TestParseDeployEnvFlagsPrintSudoersNeedsNoHost(t *testing.T) {
	cfg, err := parseDeployEnvFlags([]string{"--print-sudoers"})
	if err != nil {
		t.Fatalf("parseDeployEnvFlags(--print-sudoers) error = %v", err)
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

// Carry-over (a) at the CLI seam: --print-sudoers refuses unsafe service
// names and roots, which would otherwise inject sudoers lines.
func TestRunDeploySSHPrintSudoersRejectsUnsafeValues(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{
		"deploy", "ssh", "--print-sudoers", "--dir", dir, "--path", "/opt/x\ny ALL=(root) NOPASSWD: ALL",
	})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("unsafe --path error = %v, want ErrDeploy", err)
	}
	if strings.Contains(out.String(), "NOPASSWD: ALL") {
		t.Fatalf("unsafe sudoers content was rendered:\n%s", out.String())
	}
}

func TestParseDeployEnvFlagsUnitSandbox(t *testing.T) {
	for _, args := range [][]string{
		{"--unit-sandbox", "off"},
		{"--unit-sandbox=off"},
	} {
		cfg, err := parseDeployEnvFlags(args)
		if err != nil {
			t.Fatalf("parseDeployEnvFlags(%v) error = %v", args, err)
		}
		if cfg.UnitSandbox != "off" {
			t.Fatalf("UnitSandbox = %q, want off", cfg.UnitSandbox)
		}
	}
	cfg, err := parseDeployEnvFlags([]string{"--unit-sandbox", "on"})
	if err != nil || cfg.UnitSandbox != "on" {
		t.Fatalf("parseDeployEnvFlags(--unit-sandbox on) = %+v, %v", cfg, err)
	}
}
