package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// The rollback engine itself is tested in internal/deploy/rollback_test.go;
// this file covers the CLI surface: routing, flag parsing (no build flags),
// env resolution reuse, the prod confirmation, and help.

func TestParseDeployRollbackFlagsAcceptsAllOptions(t *testing.T) {
	cfg, err := parseDeployRollbackFlags([]string{
		"--host", "ops@server",
		"--user", "deploy",
		"--port", "2222",
		"--path", "/srv/app",
		"--service", "demo.service",
		"--dir", "/tmp/proj",
		"--env-file", ".env.ci",
		"--identity", "/keys/ci_ed25519",
		"--yes",
		"--allow-shared-admin",
	})
	if err != nil {
		t.Fatalf("parseDeployRollbackFlags() error = %v", err)
	}
	want := deployRollbackConfig{
		Host: "ops@server", User: "deploy", Port: 2222,
		Path: "/srv/app", Service: "demo.service", Dir: "/tmp/proj",
		EnvFile: ".env.ci", Identity: "/keys/ci_ed25519",
		Yes: true, AllowSharedAdmin: true,
	}
	if cfg != want {
		t.Fatalf("parseDeployRollbackFlags() = %+v, want %+v", cfg, want)
	}

	// --flag=value forms parse identically; Dir defaults to ".".
	cfg2, err := parseDeployRollbackFlags([]string{"--host=ops@server", "--port=2222"})
	if err != nil {
		t.Fatalf("parseDeployRollbackFlags(inline) error = %v", err)
	}
	if cfg2.Host != "ops@server" || cfg2.Port != 2222 || cfg2.Dir != "." {
		t.Fatalf("inline forms = %+v", cfg2)
	}
}

// Rollback never builds: the build-related deploy flags are refused with a
// pointed message, and bad values are usage errors.
func TestParseDeployRollbackFlagsRejectsBuildFlagsAndBadValues(t *testing.T) {
	for _, args := range [][]string{
		{"--target", "linux/arm64"},
		{"--keep", "3"},
		{"--unit-sandbox", "off"},
		{"--print-sudoers"},
	} {
		_, err := parseDeployRollbackFlags(args)
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "rollback never builds") {
			t.Fatalf("parseDeployRollbackFlags(%v) error = %v, want build-flag refusal", args, err)
		}
	}
	for _, args := range [][]string{
		{"--port", "abc"},
		{"--port", "0"},
		{"--bogus"},
		{"--host"},
	} {
		if _, err := parseDeployRollbackFlags(args); !errors.Is(err, ErrUsage) {
			t.Fatalf("parseDeployRollbackFlags(%v) error = %v, want ErrUsage", args, err)
		}
	}
}

func TestDeployRollbackRequiresEnvOrHost(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "rollback"})
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("deploy rollback (no env/host) error = %v, want ErrUsage naming --host", err)
	}
}

// `deploy rollback <env>` resolves the environment exactly like `deploy
// <env>`: unknown environments list the known ones, and a resolvable env
// reaches the engine's trust gate (no host pinned in this fixture).
func TestDeployRollbackResolvesEnvironment(t *testing.T) {
	dir := writeDeployProject(t, deployProjectYAML)

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "rollback", "nosuch", "--dir", dir})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("deploy rollback nosuch error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"deploy.nosuch", "staging", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want mention of %q", err, want)
		}
	}

	err = app.Run(context.Background(), []string{"deploy", "rollback", "staging", "--dir", dir})
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust") {
		t.Fatalf("deploy rollback staging error = %v, want trust-gate failure (env resolved)", err)
	}
}

func TestDeployRollbackRequiresPipYAML(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err := app.Run(context.Background(), []string{"deploy", "rollback", "staging", "--dir", t.TempDir()})
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "pip.yaml") {
		t.Fatalf("deploy rollback without pip.yaml error = %v, want ErrDeploy naming pip.yaml", err)
	}
}

// Acceptance: rolling back prod/production demands --yes or an interactive
// confirmation; declining aborts before any work.
func TestDeployRollbackProdRequiresConfirmation(t *testing.T) {
	dir := writeDeployProject(t, deployProjectYAML)

	t.Run("non-interactive without --yes", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(nil, &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "rollback", "prod", "--dir", dir})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("error = %v, want confirmation requirement naming --yes", err)
		}
	})

	t.Run("interactive decline", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(strings.NewReader("n\n"), &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "rollback", "prod", "--dir", dir})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("error = %v, want abort after decline", err)
		}
		if !strings.Contains(out.String(), "Roll back prod") || !strings.Contains(out.String(), "[y/N]") {
			t.Fatalf("missing confirmation prompt:\n%s", out.String())
		}
	})

	t.Run("interactive accept proceeds", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(strings.NewReader("y\n"), &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "rollback", "prod", "--dir", dir})
		// No host is pinned: an accepted confirm reaches the engine's trust
		// gate, proving the confirmation passed.
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust") {
			t.Fatalf("error = %v, want trust-gate failure after accepted confirm", err)
		}
	})

	t.Run("--yes skips the prompt", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New("dev", WithStreams(nil, &out, &errOut))
		err := app.Run(context.Background(), []string{"deploy", "rollback", "prod", "--dir", dir, "--yes"})
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
		err := app.Run(context.Background(), []string{"deploy", "rollback", "staging", "--dir", dir})
		if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust") {
			t.Fatalf("error = %v, want trust-gate failure without any prompt", err)
		}
	})
}

func TestRunDeployRollbackHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"deploy", "rollback", "--help"}); err != nil {
		t.Fatalf("Run(deploy rollback --help) error = %v", err)
	}
	for _, want := range []string{
		"Usage: ouvrier deploy rollback <env>",
		"deploys.log",
		"--host", "--env-file", "--identity", "--yes",
		// The .env decision is documented where operators will look first.
		"NOT rolled back",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deploy rollback help missing %q in:\n%s", want, out.String())
		}
	}
	// Build flags are absent from the offered options (the prose may still
	// explain that pruning by --keep can remove the rollback target).
	_, options, found := strings.Cut(out.String(), "Options:")
	if !found {
		t.Fatalf("deploy rollback help has no Options block:\n%s", out.String())
	}
	for _, banned := range []string{"--target", "--keep", "--unit-sandbox", "--print-sudoers"} {
		if strings.Contains(options, banned) {
			t.Fatalf("deploy rollback help must not offer %q:\n%s", banned, options)
		}
	}
}

// The top-level deploy help names the rollback subcommand.
func TestDeployHelpMentionsRollback(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"deploy", "--help"}); err != nil {
		t.Fatalf("Run(deploy --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "ouvrier deploy rollback <env>") {
		t.Fatalf("deploy help missing the rollback subcommand:\n%s", out.String())
	}
}
