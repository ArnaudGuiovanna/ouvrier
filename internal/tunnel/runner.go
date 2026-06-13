package tunnel

// runner.go holds the tunnelRunner seam: the interface for spawning the
// long-lived `ssh -N` forward process, mirroring internal/deploy's
// RemoteRunner spirit but with Start/Wait/Kill semantics and stderr capture
// instead of run-to-completion. Tests substitute a fake that drives the
// manager's state transitions without spawning ssh.

import (
	"fmt"
	"os/exec"
	"strconv"
	"sync"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// Forward describes one local-to-remote port forward.
type Forward struct {
	// Network is "unix" (default: forward from a local unix socket) or "tcp"
	// (--tcp-tunnels fallback: forward from an ephemeral loopback port).
	Network string
	// LocalAddr is the unix socket path or the 127.0.0.1:port literal.
	LocalAddr string
	// RemoteAddr is the host:port the remote sshd dials, i.e. the worker's
	// loopback admin listener from Deployment.AdminAddr.
	RemoteAddr string
}

// process is one running ssh -N forward process. Wait may be called at most
// once; Kill and Stderr are safe from any goroutine.
type process interface {
	// Wait blocks until the process exits and returns its exit error.
	Wait() error
	// Kill terminates the process. Wait still reaps it afterwards.
	Kill() error
	// Stderr returns the stderr captured so far, verbatim.
	Stderr() string
}

// tunnelRunner spawns the long-lived ssh -N process for one forward. It is an
// interface so lifecycle tests can substitute a fake without spawning ssh.
type tunnelRunner interface {
	Start(opts deploy.ConnectOpts, fwd Forward) (process, error)
}

// sshRunner shells out to the system OpenSSH client.
type sshRunner struct{}

// sshArgs builds the argv for the long-lived forward process:
//
//	ssh -N -o BatchMode=yes -o ExitOnForwardFailure=yes
//	    -o ServerAliveInterval=15 <hardening+pinning opts>
//	    -L <local>:<remote host:port> [-i identity] [-p port] user@host
//
// BatchMode, strict pinned host-key checking, and the no-password options
// come from deploy.ConnectionHardeningArgs so tunnel invocations share the
// exact hardening of every other remote invocation. Note there is no remote
// command and no token anywhere in the argv.
func sshArgs(opts deploy.ConnectOpts, fwd Forward) []string {
	args := []string{
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
	}
	if fwd.Network == "unix" {
		// The socket lives in a 0700 dir already; the mask makes the socket
		// itself 0600 too.
		args = append(args, "-o", "StreamLocalBindMask=0177")
	}
	if opts.Port != 0 {
		args = append(args, "-p", strconv.Itoa(opts.Port))
	}
	args = append(args, deploy.ConnectionHardeningArgs(opts)...)
	args = append(args, "-L", fwd.LocalAddr+":"+fwd.RemoteAddr)
	return append(args, opts.UserHost())
}

func (sshRunner) Start(opts deploy.ConnectOpts, fwd Forward) (process, error) {
	cmd := exec.Command("ssh", sshArgs(opts, fwd)...)
	stderr := &tailBuffer{limit: 16 * 1024}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: spawn ssh: %w", ErrTunnel, err)
	}
	return &sshProcess{cmd: cmd, stderr: stderr}, nil
}

// sshProcess wraps the exec.Cmd of one running ssh forward.
type sshProcess struct {
	cmd    *exec.Cmd
	stderr *tailBuffer
}

func (p *sshProcess) Wait() error    { return p.cmd.Wait() }
func (p *sshProcess) Kill() error    { return p.cmd.Process.Kill() }
func (p *sshProcess) Stderr() string { return p.stderr.String() }

// tailBuffer keeps the last limit bytes written, so a chatty or long-lived
// ssh process cannot grow stderr capture without bound while the most recent
// (and most diagnostic) lines are preserved verbatim.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
