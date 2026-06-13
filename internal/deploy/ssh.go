package deploy

// ssh.go holds the SSH transport seam shared by the deploy engine: the
// RemoteRunner interface and its OpenSSH-backed default implementation,
// connection hardening (BatchMode, strict pinned host-key checking, no
// password authentication, ever), host-key pinning against the committed
// ouvrier.known_hosts, shell quoting, and admin-token masking. The deploy
// orchestration itself lives in deploy_env.go.

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
	// SSHIn executes a remote shell command with stdin attached to the given
	// bytes. The health gate uses it to feed the admin token to `curl -K -`
	// as a curl config over the ssh channel: the token never appears in any
	// argv (local or remote, so never in `ps`) and never touches a disk.
	SSHIn(ctx context.Context, opts ConnectOpts, command string, stdin []byte) (stdout string, err error)
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

func (r *pinnedRunner) SSHIn(ctx context.Context, opts ConnectOpts, command string, stdin []byte) (string, error) {
	stdout, err := r.inner.SSHIn(ctx, opts, command, stdin)
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
	stdout, _, err := runHostCommand(ctx, "ssh", args, nil)
	return stdout, err
}

func (defaultRemoteRunner) SSHIn(ctx context.Context, opts ConnectOpts, command string, stdin []byte) (string, error) {
	args := sshBaseArgs(opts)
	args = append(args, opts.userHost(), command)
	stdout, _, err := runHostCommand(ctx, "ssh", args, stdin)
	return stdout, err
}

func (defaultRemoteRunner) SCP(ctx context.Context, opts ConnectOpts, localPath, remotePath string) error {
	args := scpBaseArgs(opts)
	args = append(args, localPath, opts.userHost()+":"+remotePath)
	_, _, err := runHostCommand(ctx, "scp", args, nil)
	return err
}

func (defaultRemoteRunner) SCPData(ctx context.Context, opts ConnectOpts, data []byte, remotePath string) error {
	// Stream the bytes to the remote host via a local temp file + scp; this
	// keeps the implementation focused on what tests need to substitute.
	// Callers must not ship secrets through this path (the deploy env payload
	// goes through it deliberately: it is the secrets file upload itself).
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
// stdout and stderr separately and attaching stdin when non-nil. Only the
// default remote runner uses it.
func runHostCommand(ctx context.Context, name string, args []string, stdin []byte) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		// Deliberately omit the command arguments: for ssh they include the
		// full remote command. Program name, exit status, and stderr are
		// enough to diagnose failures.
		return stdout.String(), stderr.String(), fmt.Errorf("%s: %w (stderr=%s)", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}
