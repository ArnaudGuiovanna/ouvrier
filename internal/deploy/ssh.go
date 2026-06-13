package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrDeploy is returned when a deploy cannot proceed (bad options, missing
// files, transport failures, or rollback after a failed health check).
var ErrDeploy = errors.New("deploy error")

// Opts captures the resolved options for an SSH deploy.
type Opts struct {
	Dir        string // project directory; defaults to "."
	Host       string // remote host (required); may be a ~/.ssh/config alias
	User       string
	Port       int
	Path       string // remote install path; defaults to /opt/ouvrier/<name>
	Service    string // systemd unit name; defaults to ouvrier-<name>
	HealthURL  string // path or full URL; defaults to /admin/health
	AdminToken string // masked in logs/output
	Identity   string // optional ssh identity file (-i) for agent-less CI
	// UnitSandbox is the systemd hardening toggle ("", "on" or "off",
	// --unit-sandbox / pip.yaml deploy.sandbox). It is validated here but
	// only takes effect with the hardened release-layout unit (RenderUnitFile)
	// used by the orchestrated deploy flow; the legacy unit rendered by this
	// flow has no hardening block to disable.
	UnitSandbox string

	// GoRun is the `go build` seam; nil means DefaultGoRunner.
	GoRun GoRunner
	// Runner is the ssh/scp seam; nil means the system ssh/scp binaries.
	Runner RemoteRunner
}

// ProgressWriter receives a deploy's streamed human-readable output. Out
// carries the step-by-step progress (the CLI passes its stdout); Err carries
// build tool diagnostics and warnings. Nil writers discard.
type ProgressWriter struct {
	Out io.Writer
	Err io.Writer
}

func (p ProgressWriter) normalized() ProgressWriter {
	if p.Out == nil {
		p.Out = io.Discard
	}
	if p.Err == nil {
		p.Err = io.Discard
	}
	return p
}

// RemoteRunner runs ssh/scp commands against a host. It is an interface so
// tests can substitute a fake without spawning ssh.
type RemoteRunner interface {
	// SSH executes a remote shell command via ssh, returning combined stdout.
	SSH(ctx context.Context, opts ConnectOpts, command string) (stdout string, err error)
	// SCP uploads localPath to remotePath on the host.
	SCP(ctx context.Context, opts ConnectOpts, localPath, remotePath string) error
	// SCPData uploads the given bytes as a remote file at remotePath.
	SCPData(ctx context.Context, opts ConnectOpts, data []byte, remotePath string) error
}

// ConnectOpts holds the dial information shared by SSH/SCP calls.
type ConnectOpts struct {
	Host string
	User string
	Port int
	// Identity is an optional ssh identity file passed as -i to ssh and scp.
	Identity string
	// KnownHosts is the pinned host-key file passed as
	// -o UserKnownHostsFile=... to ssh and scp. Host keys are always checked
	// strictly; an empty value falls back to the user's default known_hosts.
	KnownHosts string
}

// userHost renders the host portion in user@host form. Empty user yields
// just the host, matching ssh/scp's default behavior.
func (o ConnectOpts) userHost() string {
	if o.User == "" {
		return o.Host
	}
	return o.User + "@" + o.Host
}

// DeploySSH builds a static binary and ships it to opts.Host over SSH:
// upload binary and .env, install the systemd unit, restart the service, and
// health-check, rolling back to the previous binary on failure.
func DeploySSH(ctx context.Context, opts Opts, progress ProgressWriter) error {
	// Defense in depth: whatever layer produced the error (including a runner
	// that embeds the remote command, which carries the admin token in an
	// Authorization header), the rendered message never contains the token.
	return maskTokenErr(deploySSH(ctx, opts, progress), opts.AdminToken)
}

func deploySSH(ctx context.Context, opts Opts, progress ProgressWriter) error {
	if ctx == nil {
		ctx = context.Background()
	}
	progress = progress.normalized()
	out := progress.Out
	if strings.TrimSpace(opts.Host) == "" {
		return fmt.Errorf("%w: ssh deploy requires a host", ErrDeploy)
	}
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.HealthURL == "" {
		opts.HealthURL = "/admin/health"
	}
	if _, err := ResolveSandbox(opts.UnitSandbox, ""); err != nil {
		return err
	}
	goRun := opts.GoRun
	if goRun == nil {
		goRun = DefaultGoRunner
	}
	ssh := opts.Runner
	if ssh == nil {
		ssh = defaultRemoteRunner{}
	}

	// 0. Host-key pinning gate: the target must already be trusted via the
	//    committed ouvrier.known_hosts at the project root. This runs before
	//    the build and before any remote command so an untrusted host fails
	//    fast, and a changed-key ssh failure later is remapped to a hard
	//    error naming `ouvrier server trust --rotate`.
	knownHosts, pinnedHost, err := requirePinnedHost(opts.Dir, opts.Host, opts.Port)
	if err != nil {
		return err
	}
	ssh = &pinnedRunner{inner: ssh, host: pinnedHost}

	// 1. Build a static linux/amd64 binary the deploy can ship.
	br, err := StaticBuild(ctx, opts.Dir, progress.Out, progress.Err, goRun)
	if err != nil {
		return err
	}

	// 2. Resolve remote paths / service name now that we know the project name.
	remotePath := opts.Path
	if remotePath == "" {
		remotePath = "/opt/ouvrier/" + br.ProjectName
	}
	service := opts.Service
	if service == "" {
		service = "ouvrier-" + br.ProjectName
	}
	// Canonicalize the unit name: operators may pass --service demo.service.
	// Strip the suffix once so the unit path on disk, the install target in
	// /etc/systemd/system, and systemctl restart all agree on a single
	// ".service".
	serviceName := CanonicalServiceName(service)

	connect := ConnectOpts{
		Host:       opts.Host,
		User:       opts.User,
		Port:       opts.Port,
		Identity:   opts.Identity,
		KnownHosts: knownHosts,
	}

	// 3. Require a local .env to avoid blank deploys.
	localEnv := filepath.Join(br.Dir, ".env")
	if _, statErr := os.Stat(localEnv); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: no .env in %s; refuse to deploy without secrets configuration", ErrDeploy, br.Dir)
		}
		return fmt.Errorf("%w: stat .env: %w", ErrDeploy, statErr)
	}

	// 4. Ensure remote directories exist.
	mkdirCmd := fmt.Sprintf("mkdir -p %s/bin", shellQuote(remotePath))
	fmt.Fprintf(out, "ssh %s: %s\n", connect.userHost(), mkdirCmd)
	if _, err := ssh.SSH(ctx, connect, mkdirCmd); err != nil {
		return fmt.Errorf("%w: prepare remote layout: %w", ErrDeploy, err)
	}

	// 5. Upload the binary as <path>/bin/<name>.new. We swap into place after
	//    a successful health check, keeping the previous binary for rollback.
	remoteBinNew := remotePath + "/bin/" + br.ProjectName + ".new"
	fmt.Fprintf(out, "scp %s -> %s:%s\n", br.Output, connect.userHost(), remoteBinNew)
	if err := ssh.SCP(ctx, connect, br.Output, remoteBinNew); err != nil {
		return fmt.Errorf("%w: upload binary: %w", ErrDeploy, err)
	}

	// 6. Upload .env (always, since we already verified it exists locally).
	remoteEnv := remotePath + "/.env"
	fmt.Fprintf(out, "scp .env -> %s:%s\n", connect.userHost(), remoteEnv)
	if err := ssh.SCP(ctx, connect, localEnv, remoteEnv); err != nil {
		return fmt.Errorf("%w: upload .env: %w", ErrDeploy, err)
	}
	chmodCmd := fmt.Sprintf("chmod 0600 %s", shellQuote(remoteEnv))
	if _, err := ssh.SSH(ctx, connect, chmodCmd); err != nil {
		return fmt.Errorf("%w: chmod remote .env: %w", ErrDeploy, err)
	}

	// 7. Upload runtime assets that are intentionally read from disk by the
	//    worker, such as skills/<name>/SKILL.md.
	if err := uploadRuntimeAssets(ctx, ssh, connect, br.Dir, remotePath, out); err != nil {
		return err
	}

	// 8. Render and upload the systemd unit, then daemon-reload.
	unitPath := remotePath + "/" + serviceName + ".service"
	unit := renderSystemdUnit(systemdUnitParams{
		Name:        br.ProjectName,
		Service:     serviceName,
		InstallPath: remotePath,
		User:        opts.User,
	})
	fmt.Fprintf(out, "render systemd unit -> %s\n", unitPath)
	if err := ssh.SCPData(ctx, connect, []byte(unit), unitPath); err != nil {
		return fmt.Errorf("%w: upload systemd unit: %w", ErrDeploy, err)
	}
	systemdLink := fmt.Sprintf(
		"sudo install -m 0644 %s /etc/systemd/system/%s.service && sudo systemctl daemon-reload",
		shellQuote(unitPath), shellQuote(serviceName),
	)
	if _, err := ssh.SSH(ctx, connect, systemdLink); err != nil {
		return fmt.Errorf("%w: install systemd unit: %w", ErrDeploy, err)
	}

	// 9. Promote the new binary: keep the previous (if any) as .previous,
	//    move .new into place, then restart the service.
	remoteBin := remotePath + "/bin/" + br.ProjectName
	remoteBinPrev := remotePath + "/bin/" + br.ProjectName + ".previous"
	promoteCmd := fmt.Sprintf(
		"if [ -f %s ]; then mv %s %s; fi && mv %s %s && chmod 0755 %s",
		shellQuote(remoteBin), shellQuote(remoteBin), shellQuote(remoteBinPrev),
		shellQuote(remoteBinNew), shellQuote(remoteBin),
		shellQuote(remoteBin),
	)
	if _, err := ssh.SSH(ctx, connect, promoteCmd); err != nil {
		return fmt.Errorf("%w: promote binary: %w", ErrDeploy, err)
	}

	restartCmd := fmt.Sprintf("sudo systemctl restart %s", shellQuote(serviceName))
	if _, err := ssh.SSH(ctx, connect, restartCmd); err != nil {
		return rollbackBinary(ctx, ssh, connect, remoteBin, remoteBinPrev, serviceName, opts.AdminToken, fmt.Errorf("%w: systemctl restart failed: %w", ErrDeploy, err), out)
	}

	// 10. Health check on the remote host.
	healthCmd := buildHealthCheckCommand(opts.HealthURL, opts.AdminToken)
	if _, err := ssh.SSH(ctx, connect, healthCmd); err != nil {
		return rollbackBinary(ctx, ssh, connect, remoteBin, remoteBinPrev, serviceName, opts.AdminToken, fmt.Errorf("%w: health check failed: %w", ErrDeploy, err), out)
	}

	fmt.Fprintf(out, "deployed %s to %s:%s (service=%s)\n", br.ProjectName, connect.userHost(), remotePath, serviceName)
	return nil
}

func uploadRuntimeAssets(ctx context.Context, ssh RemoteRunner, connect ConnectOpts, localRoot, remotePath string, out io.Writer) error {
	skillsRoot := filepath.Join(localRoot, "skills")
	info, err := os.Stat(skillsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: stat runtime assets: %w", ErrDeploy, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory", ErrDeploy, skillsRoot)
	}

	return filepath.WalkDir(skillsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: read runtime asset %s: %w", ErrDeploy, path, walkErr)
		}
		rel, err := filepath.Rel(localRoot, path)
		if err != nil {
			return fmt.Errorf("%w: resolve runtime asset %s: %w", ErrDeploy, path, err)
		}
		remoteAssetPath := remotePath + "/" + filepath.ToSlash(rel)
		if entry.IsDir() {
			mkdirCmd := fmt.Sprintf("mkdir -p %s", shellQuote(remoteAssetPath))
			fmt.Fprintf(out, "ssh %s: %s\n", connect.userHost(), mkdirCmd)
			if _, err := ssh.SSH(ctx, connect, mkdirCmd); err != nil {
				return fmt.Errorf("%w: create remote runtime asset directory: %w", ErrDeploy, err)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w: stat runtime asset %s: %w", ErrDeploy, path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fmt.Fprintf(out, "scp %s -> %s:%s\n", rel, connect.userHost(), remoteAssetPath)
		if err := ssh.SCP(ctx, connect, path, remoteAssetPath); err != nil {
			return fmt.Errorf("%w: upload runtime asset %s: %w", ErrDeploy, rel, err)
		}
		return nil
	})
}

func rollbackBinary(ctx context.Context, ssh RemoteRunner, connect ConnectOpts, remoteBin, remoteBinPrev, service, adminToken string, cause error, out io.Writer) error {
	// The cause may wrap remote-command output; mask the admin token before
	// anything reaches operator terminals or CI logs.
	fmt.Fprintf(out, "deploy failed: %s; rolling back\n", maskToken(cause.Error(), adminToken))
	rollback := fmt.Sprintf(
		"if [ -f %s ]; then mv %s %s && sudo systemctl restart %s; fi",
		shellQuote(remoteBinPrev), shellQuote(remoteBinPrev), shellQuote(remoteBin), shellQuote(service),
	)
	if _, rbErr := ssh.SSH(ctx, connect, rollback); rbErr != nil {
		return fmt.Errorf("%w (rollback also failed: %v)", cause, rbErr)
	}
	return cause
}

// maskToken returns s with every occurrence of token replaced by "***" so
// the admin token never reaches operator terminals or CI logs.
func maskToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// maskedError renders its wrapped error with the admin token masked while
// keeping the chain intact, so errors.Is(err, ErrDeploy) still works.
type maskedError struct {
	err   error
	token string
}

func (e *maskedError) Error() string { return maskToken(e.err.Error(), e.token) }
func (e *maskedError) Unwrap() error { return e.err }

// maskTokenErr wraps err so its message never contains token. It returns err
// unchanged when there is nothing to mask.
func maskTokenErr(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return &maskedError{err: err, token: token}
}

// buildHealthCheckCommand renders the remote curl that probes the worker's
// admin health endpoint. The admin token is interpolated into the command but
// never echoed locally and never returned via err strings.
func buildHealthCheckCommand(healthURL, adminToken string) string {
	url := healthURL
	if strings.HasPrefix(url, "/") {
		url = "http://127.0.0.1:8080" + url
	}
	auth := ""
	if adminToken != "" {
		auth = fmt.Sprintf(" -H %s", shellQuote("Authorization: Bearer "+adminToken))
	}
	return fmt.Sprintf("curl -fsS --max-time 5%s %s", auth, shellQuote(url))
}

// systemdUnitParams collects values used to render the legacy single-binary
// flow's systemd unit file. The unit is intentionally simple: it runs the
// binary out of the install directory, loads .env via EnvironmentFile, and
// restarts on failure. The release-layout deploy flow (#45) uses the
// hardened RenderUnitFile in systemd_unit.go instead; this one stays only
// until DeploySSH is migrated to the release layout, because its unit points
// at paths (<path>/bin/<name>, <path>/.env) that only this flow creates.
type systemdUnitParams struct {
	Name        string
	Service     string
	InstallPath string
	User        string
}

func renderSystemdUnit(p systemdUnitParams) string {
	user := p.User
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	return fmt.Sprintf(`[Unit]
Description=Ouvrier worker %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
WorkingDirectory=%s
EnvironmentFile=%s/.env
ExecStart=%s/bin/%s
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
`, p.Name, user, p.InstallPath, p.InstallPath, p.InstallPath, p.Name)
}

// requirePinnedHost enforces host-key pinning: the committed
// ouvrier.known_hosts at the project root must already hold an entry for the
// deploy target. It returns the absolute known_hosts path and the canonical
// hostname used for pinning.
func requirePinnedHost(dir, host string, port int) (string, string, error) {
	knownHosts, err := filepath.Abs(filepath.Join(dir, KnownHostsFile))
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve %s: %w", ErrDeploy, KnownHostsFile, err)
	}
	pinnedHost := KnownHostsHostname(host, port)
	trusted, err := HostTrusted(knownHosts, pinnedHost)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrDeploy, err)
	}
	if !trusted {
		return "", "", fmt.Errorf(
			"%w: host %s is not pinned in %s; run `ouvrier server trust %s` and commit the file",
			ErrDeploy, pinnedHost, knownHosts, trustCommandHost(host, port),
		)
	}
	return knownHosts, pinnedHost, nil
}

// trustCommandHost renders the `ouvrier server trust` invocation that would
// pin the deploy target, including --port when non-default.
func trustCommandHost(host string, port int) string {
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if port != 0 && port != 22 {
		return fmt.Sprintf("%s --port %d", host, port)
	}
	return host
}

// pinnedRunner decorates a RemoteRunner so a host-key verification failure —
// the server's key changed since it was pinned — surfaces as a hard error
// naming `ouvrier server trust --rotate` instead of a raw ssh failure.
type pinnedRunner struct {
	inner RemoteRunner
	host  string
}

func (r *pinnedRunner) SSH(ctx context.Context, opts ConnectOpts, command string) (string, error) {
	stdout, err := r.inner.SSH(ctx, opts, command)
	return stdout, remapHostKeyErr(err, r.host)
}

func (r *pinnedRunner) SCP(ctx context.Context, opts ConnectOpts, localPath, remotePath string) error {
	return remapHostKeyErr(r.inner.SCP(ctx, opts, localPath, remotePath), r.host)
}

func (r *pinnedRunner) SCPData(ctx context.Context, opts ConnectOpts, data []byte, remotePath string) error {
	return remapHostKeyErr(r.inner.SCPData(ctx, opts, data, remotePath), r.host)
}

// remapHostKeyErr turns OpenSSH's host-key verification failures into a hard
// error telling the operator to re-pin deliberately.
func remapHostKeyErr(err error, host string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "Host key verification failed") ||
		strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED") {
		return fmt.Errorf(
			"%w: host key for %s no longer matches the pinned entry in %s; if the server was reinstalled on purpose, run `ouvrier server trust --rotate %s` and commit the file: %w",
			ErrDeploy, host, KnownHostsFile, host, err,
		)
	}
	return err
}

// shellQuote returns a single-quoted POSIX shell literal. Inner single quotes
// are encoded using the standard '\” trick. This is safer than relying on
// the caller to pick characters compatible with bare ssh strings.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// defaultRemoteRunner shells out to the system ssh/scp binaries.
type defaultRemoteRunner struct{}

func (defaultRemoteRunner) SSH(ctx context.Context, opts ConnectOpts, command string) (string, error) {
	args := sshBaseArgs(opts)
	args = append(args, opts.userHost(), command)
	stdout, _, err := runHostCommand(ctx, "ssh", args)
	return stdout, err
}

func (defaultRemoteRunner) SCP(ctx context.Context, opts ConnectOpts, localPath, remotePath string) error {
	args := scpBaseArgs(opts)
	args = append(args, localPath, opts.userHost()+":"+remotePath)
	_, _, err := runHostCommand(ctx, "scp", args)
	return err
}

func (defaultRemoteRunner) SCPData(ctx context.Context, opts ConnectOpts, data []byte, remotePath string) error {
	// Stream the bytes to the remote host via ssh; this avoids writing the
	// rendered unit file to disk locally and keeps the implementation focused
	// on what tests need to substitute.
	tmp, err := os.CreateTemp("", "ouvrier-deploy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return defaultRemoteRunner{}.SCP(ctx, opts, tmp.Name(), remotePath)
}

func sshBaseArgs(opts ConnectOpts) []string {
	args := make([]string, 0, 16)
	if opts.Port != 0 {
		args = append(args, "-p", fmt.Sprintf("%d", opts.Port))
	}
	return append(args, connectionHardeningArgs(opts)...)
}

func scpBaseArgs(opts ConnectOpts) []string {
	args := make([]string, 0, 16)
	if opts.Port != 0 {
		args = append(args, "-P", fmt.Sprintf("%d", opts.Port))
	}
	return append(args, connectionHardeningArgs(opts)...)
}

// connectionHardeningArgs are the non-negotiable ssh/scp options shared by
// every remote invocation: strict pinned host-key checking and no interactive
// or password-based authentication path, ever.
func connectionHardeningArgs(opts ConnectOpts) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
	}
	if opts.KnownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+opts.KnownHosts)
	}
	if opts.Identity != "" {
		args = append(args, "-i", opts.Identity)
	}
	return args
}

// runHostCommand invokes a command via the standard library, capturing
// stdout and stderr separately. Only the default remote runner uses it.
func runHostCommand(ctx context.Context, name string, args []string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Deliberately omit the command arguments: for ssh they include the
		// full remote command, which can carry secrets such as the admin
		// bearer token used by the health check. Program name, exit status,
		// and stderr are enough to diagnose failures.
		return stdout.String(), stderr.String(), fmt.Errorf("%s: %w (stderr=%s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}
