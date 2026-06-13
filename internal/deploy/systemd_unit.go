package deploy

// systemd_unit.go renders the hardened systemd unit for the release-layout
// deploy target (issue #44): a fixed, auditable host layout under
// /opt/ouvrier/<name> with a dedicated nologin system user. This renderer is
// the single source for the deployed unit; deploy_env.go installs it when
// its sha256 differs from the unit already on the host.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// UnitParams collects the values needed to render the release-layout
// systemd unit. Zero values resolve to the documented defaults.
type UnitParams struct {
	// Name is the worker name from pip.yaml `name:` (required).
	Name string
	// Service is the unit name; empty means "ouvrier-<Name>". A trailing
	// ".service" is stripped so callers may pass either form.
	Service string
	// Root is the remote install root; empty means "/opt/ouvrier/<Name>".
	Root string
	// SandboxOff disables the hardening block (NoNewPrivileges,
	// ProtectSystem=strict, ...). Escape hatch for workers that shell out to
	// tools incompatible with the sandbox; pip.yaml `deploy.sandbox: off` or
	// `--unit-sandbox=off`.
	SandboxOff bool
}

// normalized fills in the documented defaults.
func (p UnitParams) normalized() UnitParams {
	p.Service = CanonicalServiceName(p.Service)
	if p.Service == "" {
		p.Service = "ouvrier-" + p.Name
	}
	if p.Root == "" {
		p.Root = "/opt/ouvrier/" + p.Name
	}
	return p
}

// Validate rejects UnitParams values that could break out of the rendered
// systemd unit, the shell command helpers, or the sudoers snippet (whose
// grants are unquoted): whitespace or control characters (a newline in the
// root path would inject unit directives or sudoers lines), shell quotes, and
// relative roots. Callers must validate before rendering or running any
// remote command built from these values.
func (p UnitParams) Validate() error {
	p = p.normalized()
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("%w: worker name is required", ErrDeploy)
	}
	for _, f := range []struct{ label, value string }{
		{"worker name", p.Name},
		{"service name", p.Service},
		{"install root", p.Root},
	} {
		if err := rejectUnsafeUnitValue(f.label, f.value); err != nil {
			return err
		}
	}
	if !strings.HasPrefix(p.Root, "/") {
		return fmt.Errorf("%w: install root %q must be an absolute path", ErrDeploy, p.Root)
	}
	return nil
}

// rejectUnsafeUnitValue refuses whitespace, control characters, and quote
// characters in values that end up in systemd unit files, sudoers grants, and
// remote shell commands.
func rejectUnsafeUnitValue(label, value string) error {
	for _, r := range value {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r < 0x20 || r == 0x7f || r == '\'' || r == '"' || r == '\\' {
			return fmt.Errorf("%w: %s %q must not contain whitespace, quotes, or control characters", ErrDeploy, label, value)
		}
	}
	return nil
}

// CanonicalServiceName strips a single trailing ".service" so the unit file
// on disk, the install target in /etc/systemd/system, and systemctl
// invocations all agree on one ".service" suffix (operators may pass
// --service demo.service).
func CanonicalServiceName(service string) string {
	return strings.TrimSuffix(service, ".service")
}

// UnitUser returns the dedicated per-worker system user the unit runs as.
// The deploy flow creates it with useradd --system --shell nologin; the unit
// never runs as root.
func UnitUser(name string) string {
	return "ouvrier-" + name
}

// RenderUnitFile renders the /etc/systemd/system/<service>.service contents
// for the release layout. Key properties:
//
//   - User= is the dedicated ouvrier-<name> system user, never root.
//   - Environment=OUVRIER_STATE_PATH=<root>/shared/state/state.db comes
//     BEFORE EnvironmentFile=<root>/shared/.env, so the durable default
//     survives release swaps while the operator's .env can still override it
//     (e.g. to point at Postgres).
//   - ExecStart runs the binary through the <root>/current symlink, so a
//     symlink swap plus restart activates a release.
//   - Restart=always with a start limit window keeps crash loops bounded.
//   - Hardening is on by default; UnitParams.SandboxOff removes the block.
func RenderUnitFile(p UnitParams) string {
	p = p.normalized()
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=Ouvrier worker %s
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s/current
Environment=%s=%s/shared/state/state.db
EnvironmentFile=%s/shared/.env
ExecStart=%s/current/bin/%s
Restart=always
RestartSec=2s
`,
		p.Name,
		UnitUser(p.Name), UnitUser(p.Name),
		p.Root,
		envnames.StatePath, p.Root,
		p.Root,
		p.Root, p.Name,
	)
	if !p.SandboxOff {
		fmt.Fprintf(&b, `NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
UMask=0077
ReadWritePaths=%s/shared
`, p.Root)
	}
	b.WriteString(`
[Install]
WantedBy=multi-user.target
`)
	return b.String()
}

// UnitSHA256 returns the hex sha256 of a rendered unit, used to install the
// unit only when its content actually changed.
func UnitSHA256(unit string) string {
	sum := sha256.Sum256([]byte(unit))
	return hex.EncodeToString(sum[:])
}

// StagedUnitPath is where the deploy uploads the rendered unit before
// installing it into /etc/systemd/system with sudo. Staging under <root>
// keeps the privileged install command's source path fixed, so the sudoers
// snippet can pin it exactly.
func StagedUnitPath(root, service string) string {
	return root + "/" + CanonicalServiceName(service) + ".service"
}

// InstalledUnitPath is the final unit location under /etc/systemd/system.
func InstalledUnitPath(service string) string {
	return "/etc/systemd/system/" + CanonicalServiceName(service) + ".service"
}

// InstallUnitIfChangedCommand returns the remote command that installs the
// staged unit only when the installed unit's sha256 differs from sha (the
// sha256 of the rendered unit, from UnitSHA256). A changed unit triggers
// daemon-reload; an unchanged unit is a no-op so repeat deploys skip the
// privileged install entirely.
func InstallUnitIfChangedCommand(root, service, sha string) string {
	staged := StagedUnitPath(root, service)
	installed := InstalledUnitPath(service)
	return fmt.Sprintf(
		`if [ "$(sha256sum %s 2>/dev/null | cut -d' ' -f1)" != %s ]; then sudo /usr/bin/install -m 0644 -- %s %s && sudo /usr/bin/systemctl daemon-reload; fi`,
		shellQuote(installed), shellQuote(sha), shellQuote(staged), shellQuote(installed),
	)
}

// EnableServiceCommand enables the unit so it starts on boot.
func EnableServiceCommand(service string) string {
	return "sudo /usr/bin/systemctl enable " + shellQuote(CanonicalServiceName(service)+".service")
}

// RestartServiceCommand (re)starts the unit after a symlink swap.
func RestartServiceCommand(service string) string {
	return "sudo /usr/bin/systemctl restart " + shellQuote(CanonicalServiceName(service)+".service")
}

// StopServiceCommand stops the unit (first-deploy failure path: no previous
// release to roll back to, so the deploy degrades to stop+report).
func StopServiceCommand(service string) string {
	return "sudo /usr/bin/systemctl stop " + shellQuote(CanonicalServiceName(service)+".service")
}

// JournalTailCommand dumps the unit's recent log lines for failure reports.
func JournalTailCommand(service string) string {
	return "sudo /usr/bin/journalctl -u " + shellQuote(CanonicalServiceName(service)+".service") + " -n 50 --no-pager"
}

// SudoProbeCommand is the cheap remote probe verifying passwordless sudo is
// configured before the deploy does any real work. /usr/bin/true is part of
// the sudoers snippet, so the probe exercises the same policy the deploy
// relies on.
func SudoProbeCommand() string {
	return "sudo -n /usr/bin/true"
}

// SystemdCheckCommand verifies systemctl exists on the target before the
// deploy provisions anything, with an actionable stderr line. No sudo.
func SystemdCheckCommand() string {
	return "command -v systemctl >/dev/null 2>&1 || { echo 'ouvrier: systemd (systemctl) not found on this host; the SSH deploy requires a systemd-based Linux target' >&2; exit 1; }"
}

// ResolveSandbox merges the --unit-sandbox flag (highest precedence) with
// the pip.yaml deploy.<env> sandbox value. Accepted values are "", "on" and
// "off" (case-insensitive); empty means "use the other source, default on".
func ResolveSandbox(flagValue, envValue string) (sandboxOff bool, err error) {
	for _, v := range []struct{ value, source string }{
		{flagValue, "--unit-sandbox"},
		{envValue, "pip.yaml deploy sandbox"},
	} {
		switch strings.ToLower(strings.TrimSpace(v.value)) {
		case "":
			continue
		case "on":
			return false, nil
		case "off":
			return true, nil
		default:
			return false, fmt.Errorf("%w: %s must be \"on\" or \"off\", got %q", ErrDeploy, v.source, v.value)
		}
	}
	return false, nil
}
