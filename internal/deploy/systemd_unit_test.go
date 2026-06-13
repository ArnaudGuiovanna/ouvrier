package deploy

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -update rewrites the golden files in testdata/ from the current renderer
// output: go test ./internal/deploy -run Golden -update
var update = flag.Bool("update", false, "rewrite golden files in testdata/")

func TestRenderUnitFileGolden(t *testing.T) {
	cases := []struct {
		name   string
		golden string
		params UnitParams
	}{
		{
			name:   "default",
			golden: "unit_default.service",
			params: UnitParams{Name: "demo"},
		},
		{
			name:   "sandbox off",
			golden: "unit_sandbox_off.service",
			params: UnitParams{Name: "demo", SandboxOff: true},
		},
		{
			name:   "custom path and service",
			golden: "unit_custom.service",
			params: UnitParams{Name: "demo", Service: "custom-worker.service", Root: "/srv/workers/demo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderUnitFile(tc.params)
			goldenPath := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatalf("mkdir testdata: %v", err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("update golden %s: %v", goldenPath, err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s (run with -update to create): %v", goldenPath, err)
			}
			if got != string(want) {
				t.Fatalf("RenderUnitFile(%+v) does not match %s:\n--- got ---\n%s\n--- want ---\n%s", tc.params, goldenPath, got, want)
			}
		})
	}
}

// Acceptance: Environment=OUVRIER_STATE_PATH comes BEFORE EnvironmentFile so
// the operator's .env can override the durable default (e.g. to Postgres).
func TestRenderUnitFileEnvOrdering(t *testing.T) {
	for _, p := range []UnitParams{
		{Name: "demo"},
		{Name: "demo", SandboxOff: true},
		{Name: "demo", Service: "custom", Root: "/srv/x"},
	} {
		unit := RenderUnitFile(p)
		envIdx := strings.Index(unit, "Environment=OUVRIER_STATE_PATH=")
		fileIdx := strings.Index(unit, "EnvironmentFile=")
		if envIdx < 0 || fileIdx < 0 {
			t.Fatalf("unit missing Environment=/EnvironmentFile= lines (params %+v):\n%s", p, unit)
		}
		if envIdx > fileIdx {
			t.Fatalf("Environment= must come before EnvironmentFile= (params %+v):\n%s", p, unit)
		}
	}
}

func TestRenderUnitFileNeverRunsAsRoot(t *testing.T) {
	unit := RenderUnitFile(UnitParams{Name: "demo"})
	if strings.Contains(unit, "User=root") {
		t.Fatalf("unit must never run as root:\n%s", unit)
	}
	if !strings.Contains(unit, "User=ouvrier-demo\n") || !strings.Contains(unit, "Group=ouvrier-demo\n") {
		t.Fatalf("unit must run as the dedicated ouvrier-demo system user:\n%s", unit)
	}
}

func TestRenderUnitFileRestartAndStartLimit(t *testing.T) {
	unit := RenderUnitFile(UnitParams{Name: "demo"})
	for _, want := range []string{
		"Restart=always\n",
		"RestartSec=2s\n",
		"StartLimitIntervalSec=60\n",
		"StartLimitBurst=5\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestRenderUnitFileSandboxOffDropsOnlyHardening(t *testing.T) {
	hardened := RenderUnitFile(UnitParams{Name: "demo"})
	relaxed := RenderUnitFile(UnitParams{Name: "demo", SandboxOff: true})
	hardening := []string{
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"UMask=0077",
		"ReadWritePaths=/opt/ouvrier/demo/shared",
	}
	for _, h := range hardening {
		if !strings.Contains(hardened, h) {
			t.Fatalf("hardened unit missing %q:\n%s", h, hardened)
		}
		if strings.Contains(relaxed, h) {
			t.Fatalf("sandbox-off unit still contains %q:\n%s", h, relaxed)
		}
	}
	// Everything that is not hardening must survive the escape hatch.
	for _, keep := range []string{
		"User=ouvrier-demo",
		"Environment=OUVRIER_STATE_PATH=/opt/ouvrier/demo/shared/state/state.db",
		"EnvironmentFile=/opt/ouvrier/demo/shared/.env",
		"ExecStart=/opt/ouvrier/demo/current/bin/demo",
		"Restart=always",
	} {
		if !strings.Contains(relaxed, keep) {
			t.Fatalf("sandbox-off unit lost %q:\n%s", keep, relaxed)
		}
	}
}

// The binary path runs through the current symlink and the env file lives in
// shared/, so a release swap activates without rewriting the unit.
func TestRenderUnitFileUsesCurrentSymlinkAndSharedEnv(t *testing.T) {
	unit := RenderUnitFile(UnitParams{Name: "demo", Root: "/srv/workers/demo"})
	for _, want := range []string{
		"ExecStart=/srv/workers/demo/current/bin/demo\n",
		"WorkingDirectory=/srv/workers/demo/current\n",
		"EnvironmentFile=/srv/workers/demo/shared/.env\n",
		"Environment=OUVRIER_STATE_PATH=/srv/workers/demo/shared/state/state.db\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestCanonicalServiceName(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"demo":                 "demo",
		"demo.service":         "demo",
		"demo.service.service": "demo.service", // strip exactly one suffix
		"ouvrier-demo.service": "ouvrier-demo",
	}
	for in, want := range cases {
		if got := CanonicalServiceName(in); got != want {
			t.Fatalf("CanonicalServiceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnitSHA256IsStableAndContentSensitive(t *testing.T) {
	a := RenderUnitFile(UnitParams{Name: "demo"})
	b := RenderUnitFile(UnitParams{Name: "demo"})
	c := RenderUnitFile(UnitParams{Name: "demo", SandboxOff: true})
	if UnitSHA256(a) != UnitSHA256(b) {
		t.Fatal("UnitSHA256 must be deterministic for identical units")
	}
	if UnitSHA256(a) == UnitSHA256(c) {
		t.Fatal("UnitSHA256 must differ for different unit content")
	}
	if len(UnitSHA256(a)) != 64 {
		t.Fatalf("UnitSHA256 length = %d, want 64 hex chars", len(UnitSHA256(a)))
	}
}

// The unit is installed only when the installed copy's sha256 differs from
// the rendered one; an unchanged unit skips both the privileged install and
// the daemon-reload.
func TestInstallUnitIfChangedCommand(t *testing.T) {
	unit := RenderUnitFile(UnitParams{Name: "demo"})
	sha := UnitSHA256(unit)
	cmd := InstallUnitIfChangedCommand("/opt/ouvrier/demo", "ouvrier-demo.service", sha)
	for _, want := range []string{
		`sha256sum '/etc/systemd/system/ouvrier-demo.service'`,
		"!= '" + sha + "'",
		`sudo /usr/bin/install -m 0644 -- '/opt/ouvrier/demo/ouvrier-demo.service' '/etc/systemd/system/ouvrier-demo.service'`,
		"sudo /usr/bin/systemctl daemon-reload",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("InstallUnitIfChangedCommand missing %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, ".service.service") {
		t.Fatalf("InstallUnitIfChangedCommand doubled the .service suffix:\n%s", cmd)
	}
}

func TestServiceLifecycleCommandsQuoteUnitName(t *testing.T) {
	cases := map[string]string{
		EnableServiceCommand("ouvrier-demo"):  "sudo /usr/bin/systemctl enable 'ouvrier-demo.service'",
		RestartServiceCommand("ouvrier-demo"): "sudo /usr/bin/systemctl restart 'ouvrier-demo.service'",
		StopServiceCommand("ouvrier-demo"):    "sudo /usr/bin/systemctl stop 'ouvrier-demo.service'",
		JournalTailCommand("ouvrier-demo"):    "sudo /usr/bin/journalctl -u 'ouvrier-demo.service' -n 50 --no-pager",
		RestartServiceCommand("demo.service"): "sudo /usr/bin/systemctl restart 'demo.service'",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("lifecycle command = %q, want %q", got, want)
		}
	}
}

func TestResolveSandbox(t *testing.T) {
	cases := []struct {
		flag, env string
		off       bool
		wantErr   bool
	}{
		{"", "", false, false},
		{"on", "", false, false},
		{"off", "", true, false},
		{"", "off", true, false},
		{"", "on", false, false},
		{"on", "off", false, false}, // flag wins
		{"off", "on", true, false},  // flag wins
		{"OFF", "", true, false},    // case-insensitive
		{"bogus", "", false, true},
		{"", "maybe", false, true},
	}
	for _, tc := range cases {
		off, err := ResolveSandbox(tc.flag, tc.env)
		if tc.wantErr {
			if err == nil || !errors.Is(err, ErrDeploy) {
				t.Fatalf("ResolveSandbox(%q, %q) error = %v, want ErrDeploy", tc.flag, tc.env, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ResolveSandbox(%q, %q) error = %v", tc.flag, tc.env, err)
		}
		if off != tc.off {
			t.Fatalf("ResolveSandbox(%q, %q) = %v, want %v", tc.flag, tc.env, off, tc.off)
		}
	}
}

// Acceptance: the sudoers snippet contains only the commands the deploy flow
// runs. Both directions are checked: every snippet grant corresponds to a
// sudo invocation emitted by a command helper, and every helper sudo
// invocation is covered by a grant.
func TestSudoersSnippetMatchesDeployCommands(t *testing.T) {
	const (
		deployUser = "deploy"
		name       = "demo"
		service    = "ouvrier-demo"
		root       = "/opt/ouvrier/demo"
	)
	params := SudoersParams{DeployUser: deployUser, Name: name, Service: service, Root: root}
	snippet := SudoersSnippet(params)

	sha := UnitSHA256(RenderUnitFile(UnitParams{Name: name}))
	helperCommands := []string{SudoProbeCommand(), CreateServiceUserCommand(root, name)}
	helperCommands = append(helperCommands, MkdirLayoutCommands(root, name, deployUser)...)
	helperCommands = append(helperCommands, InstallEnvCommands(root, name)...)
	helperCommands = append(helperCommands,
		InstallUnitIfChangedCommand(root, service, sha),
		EnableServiceCommand(service),
		RestartServiceCommand(service),
		StopServiceCommand(service),
		JournalTailCommand(service),
	)

	// Collect every sudo invocation the helpers emit, in the unquoted argv
	// form that sudoers matches against.
	var sudoArgvs []string
	for _, cmd := range helperCommands {
		for _, part := range splitShellSegments(cmd) {
			k := strings.Index(part, "sudo ")
			if k < 0 {
				continue
			}
			argv := strings.TrimSpace(strings.TrimPrefix(part[k+len("sudo "):], "-n "))
			sudoArgvs = append(sudoArgvs, strings.ReplaceAll(argv, "'", ""))
		}
	}
	if len(sudoArgvs) == 0 {
		t.Fatal("no sudo invocations found in helper commands")
	}

	// Snippet grants, minus comments.
	var grants []string
	for _, line := range strings.Split(snippet, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		prefix := deployUser + " ALL=(root) NOPASSWD: "
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("unexpected sudoers line %q", line)
		}
		grants = append(grants, strings.TrimPrefix(line, prefix))
	}

	grantSet := map[string]bool{}
	for _, g := range grants {
		grantSet[g] = true
		if strings.Contains(g, "*") {
			t.Fatalf("sudoers grant contains a wildcard: %q", g)
		}
	}
	for _, argv := range sudoArgvs {
		if !grantSet[argv] {
			t.Fatalf("helper sudo invocation %q has no sudoers grant; grants:\n%s", argv, strings.Join(grants, "\n"))
		}
	}
	argvSet := map[string]bool{}
	for _, a := range sudoArgvs {
		argvSet[a] = true
	}
	for _, g := range grants {
		if !argvSet[g] {
			t.Fatalf("sudoers grant %q is not used by any deploy helper (least privilege violated)", g)
		}
	}
}

// splitShellSegments splits a shell command on the &&, || and ; separators
// the deploy helpers use, well enough to isolate each sudo invocation. The
// helpers never put these separators inside quoted arguments except in the
// flock inner script, which contains no sudo.
func splitShellSegments(cmd string) []string {
	return strings.FieldsFunc(strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n").Replace(cmd), func(r rune) bool {
		return r == '\n'
	})
}

func TestSudoersSnippetDefaults(t *testing.T) {
	snippet := SudoersSnippet(SudoersParams{Name: "demo"})
	for _, want := range []string{
		"deploy ALL=(root) NOPASSWD: /usr/bin/systemctl restart ouvrier-demo.service",
		"/opt/ouvrier/demo",
		"--shell /usr/sbin/nologin ouvrier-demo",
		"/usr/bin/journalctl -u ouvrier-demo.service -n 50 --no-pager",
	} {
		if !strings.Contains(snippet, want) {
			t.Fatalf("default sudoers snippet missing %q:\n%s", want, snippet)
		}
	}
	// A custom unit name with .service suffix is canonicalized once.
	custom := SudoersSnippet(SudoersParams{Name: "demo", Service: "custom.service", DeployUser: "ci"})
	if !strings.Contains(custom, "ci ALL=(root) NOPASSWD: /usr/bin/systemctl restart custom.service") {
		t.Fatalf("custom sudoers snippet wrong unit:\n%s", custom)
	}
	if strings.Contains(custom, ".service.service") {
		t.Fatalf("custom sudoers snippet doubled .service:\n%s", custom)
	}
}

func TestUnitUser(t *testing.T) {
	if got := UnitUser("demo"); got != "ouvrier-demo" {
		t.Fatalf("UnitUser(demo) = %q", got)
	}
}
