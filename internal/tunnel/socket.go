package tunnel

// socket.go owns the local end of each tunnel: the per-user runtime
// directory holding the forward sockets ($XDG_RUNTIME_DIR/ouvrier/tunnels,
// 0700), stale-socket unlinking before every spawn, the per-tunnel flock that
// keeps two managers from fighting over one socket, and the listen-then-close
// freeport allocation for the --tcp-tunnels fallback.

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// socketDir resolves the directory holding the forward sockets. The override
// (Options.SocketDir) wins; otherwise $XDG_RUNTIME_DIR/ouvrier/tunnels; with
// no runtime dir, a uid-scoped directory under the system temp dir.
func socketDir(override string) string {
	if override != "" {
		return override
	}
	if dir := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); dir != "" {
		return filepath.Join(dir, "ouvrier", "tunnels")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("ouvrier-%d", os.Getuid()), "tunnels")
}

// ensureSocketDir creates dir with 0700 permissions, tightening a
// pre-existing directory to 0700 so the sockets are never reachable by other
// local users.
func ensureSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create tunnel socket directory %s: %w", ErrTunnel, dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%w: chmod tunnel socket directory %s: %w", ErrTunnel, dir, err)
	}
	return nil
}

// validWorkerName reports whether name is safe as a socket filename: no
// separators, no traversal, nothing ssh could misparse.
func validWorkerName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return name[0] != '-' && name != "." && name != ".."
}

// maxSocketPath is a conservative bound under the kernel's sockaddr_un limit
// (108 bytes on Linux including the trailing NUL, 104 on BSDs).
const maxSocketPath = 103

// socketPath returns the unix socket path for a worker, refusing paths that
// would not fit in sockaddr_un.
func socketPath(dir, name string) (string, error) {
	p := filepath.Join(dir, name+".sock")
	if len(p) > maxSocketPath {
		return "", fmt.Errorf("%w: socket path %s exceeds %d bytes; point OUVRIER tunnels at a shorter directory (Options.SocketDir or XDG_RUNTIME_DIR)", ErrTunnel, p, maxSocketPath)
	}
	return p, nil
}

// unlinkStaleSocket removes a leftover socket file before spawning ssh, which
// refuses to bind over an existing path. Anything that is not a socket is
// left alone (refusing to delete a regular file someone planted there).
func unlinkStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: stat stale socket %s: %w", ErrTunnel, path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s exists and is not a socket; refusing to remove it", ErrTunnel, path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("%w: unlink stale socket %s: %w", ErrTunnel, path, err)
	}
	return nil
}

// lockSocket takes a non-blocking exclusive flock on the socket's sidecar
// .lock file so two processes never manage the same tunnel socket. The
// returned file must stay open for the tunnel's lifetime; closing it releases
// the lock. The lock file holds no content, ever.
func lockSocket(sockPath string) (*os.File, error) {
	lockPath := sockPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open tunnel lock %s: %w", ErrTunnel, lockPath, err)
	}
	if err := flockExclusiveNB(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: tunnel socket %s is locked by another process: %w", ErrTunnel, sockPath, err)
	}
	return f, nil
}

// unlockSocket releases and closes the flock taken by lockSocket.
func unlockSocket(f *os.File) {
	if f == nil {
		return
	}
	flockRelease(f)
	_ = f.Close()
}

// freePort allocates an ephemeral loopback port by listening and closing —
// never by parsing `ssh -L 0` output. The small bind race between close and
// ssh's own bind is inherent to the fallback and acceptable: ssh fails fast
// (ExitOnForwardFailure) and the manager retries with a fresh port.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("%w: allocate loopback port: %w", ErrTunnel, err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		return "", fmt.Errorf("%w: release loopback port: %w", ErrTunnel, err)
	}
	return addr, nil
}
