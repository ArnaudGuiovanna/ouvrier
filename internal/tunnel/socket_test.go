package tunnel

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketDirResolution(t *testing.T) {
	if got := socketDir("/explicit/dir"); got != "/explicit/dir" {
		t.Fatalf("override ignored: %s", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := socketDir(""); got != filepath.Join("/run/user/1000", "ouvrier", "tunnels") {
		t.Fatalf("XDG_RUNTIME_DIR resolution = %s", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := socketDir("")
	if !strings.Contains(got, "ouvrier-") || !strings.HasSuffix(got, "tunnels") {
		t.Fatalf("fallback resolution = %s, want uid-scoped temp dir", got)
	}
}

func TestEnsureSocketDirTightensPerms(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureSocketDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %o, want 700", perm)
	}
}

func TestValidWorkerName(t *testing.T) {
	for _, ok := range []string{"w1", "api.prod", "a_b-c", "X9"} {
		if !validWorkerName(ok) {
			t.Errorf("validWorkerName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "-x", "a/b", "a b", "a\x00b", "héllo"} {
		if validWorkerName(bad) {
			t.Errorf("validWorkerName(%q) = true, want false", bad)
		}
	}
}

func TestSocketPathLengthGuard(t *testing.T) {
	if _, err := socketPath("/tmp/x", "w1"); err != nil {
		t.Fatalf("short path rejected: %v", err)
	}
	long := "/tmp/" + strings.Repeat("d", 120)
	if _, err := socketPath(long, "w1"); err == nil {
		t.Fatal("over-long socket path accepted; sockaddr_un would truncate it")
	}
}

func TestLockSocketConflicts(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "w1.sock")
	first, err := lockSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockSocket(sock); err == nil {
		t.Fatal("second flock on the same socket succeeded; tunnels could fight")
	} else if !errors.Is(err, ErrTunnel) || !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("conflict error = %v", err)
	}
	unlockSocket(first)
	second, err := lockSocket(sock)
	if err != nil {
		t.Fatalf("relock after release failed: %v", err)
	}
	unlockSocket(second)
}

func TestFreePortAllocatesUsableLoopbackAddr(t *testing.T) {
	addr, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" || port == "0" || port == "" {
		t.Fatalf("freePort = %q (%v), want 127.0.0.1:<nonzero>", addr, err)
	}
	// The port is actually bindable right after (listen-then-close).
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("freeport %s not bindable: %v", addr, err)
	}
	_ = ln.Close()
}

func TestUnlinkStaleSocketLeavesMissingAlone(t *testing.T) {
	if err := unlinkStaleSocket(filepath.Join(t.TempDir(), "absent.sock")); err != nil {
		t.Fatalf("missing socket should be a no-op, got %v", err)
	}
}
