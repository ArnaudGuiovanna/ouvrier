//go:build !windows

package tunnel

// Opt-in integration test against a real, throwaway sshd. It is deliberately
// env-gated (CI never runs it):
//
//	OUVRIER_TEST_SSHD=1 go test ./internal/tunnel/ -run TestSSHDIntegration -v
//
// The test generates a throwaway host key and client key, starts a local
// sshd as the current user on an ephemeral loopback port (StrictModes off,
// pubkey only), pins the generated host key in a scratch ouvrier.known_hosts,
// and round-trips an HTTP request through Manager.Transport — system ssh
// process, real unix socket forward, real bearer injection — to a local HTTP
// server standing in for the worker's admin listener.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

func TestSSHDIntegration(t *testing.T) {
	if os.Getenv("OUVRIER_TEST_SSHD") != "1" {
		t.Skip("set OUVRIER_TEST_SSHD=1 to run the sshd integration test")
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		sshd = "/usr/sbin/sshd" // common location outside PATH
	}
	if _, err := os.Stat(sshd); err != nil {
		t.Skipf("sshd not found: %v", err)
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skipf("ssh-keygen not found: %v", err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	hostKey := filepath.Join(dir, "host_key")
	clientKey := filepath.Join(dir, "client_key")
	for _, key := range []string{hostKey, clientKey} {
		out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen %s: %v\n%s", key, err, out)
		}
	}
	clientPub, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	authorized := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authorized, clientPub, 0o600); err != nil {
		t.Fatal(err)
	}

	// Ephemeral sshd port via the same listen-then-close trick the tcp
	// fallback uses.
	sshdAddr, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	_, sshdPort, err := net.SplitHostPort(sshdAddr)
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "sshd_config")
	confBody := fmt.Sprintf(`Port %s
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
StrictModes no
UsePAM no
AllowTcpForwarding yes
LogLevel ERROR
`, sshdPort, hostKey, filepath.Join(dir, "sshd.pid"), authorized)
	if err := os.WriteFile(conf, []byte(confBody), 0o600); err != nil {
		t.Fatal(err)
	}

	sshdCmd := exec.Command(sshd, "-D", "-e", "-f", conf)
	var sshdLog strings.Builder
	sshdCmd.Stderr = &sshdLog
	if err := sshdCmd.Start(); err != nil {
		t.Fatalf("start sshd: %v", err)
	}
	t.Cleanup(func() {
		_ = sshdCmd.Process.Kill()
		_, _ = sshdCmd.Process.Wait()
	})
	waitDialable(t, "tcp", "127.0.0.1:"+sshdPort, 10*time.Second, &sshdLog)

	// The stand-in admin listener: loopback HTTP requiring the bearer token
	// the Manager must inject.
	const token = "itest-token-7f3a"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "tunneled")
	}))
	t.Cleanup(backend.Close)
	adminAddr := strings.TrimPrefix(backend.URL, "http://")

	// Pin the generated host key the way `ouvrier server trust` would.
	hostPub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(hostPub))
	if len(fields) < 2 {
		t.Fatalf("unexpected host pub key: %q", hostPub)
	}
	projDir := t.TempDir()
	pinned := fmt.Sprintf("[127.0.0.1]:%s %s %s\n", sshdPort, fields[0], fields[1])
	if err := os.WriteFile(filepath.Join(projDir, "ouvrier.known_hosts"), []byte(pinned), 0o644); err != nil {
		t.Fatal(err)
	}

	port := 0
	if _, err := fmt.Sscanf(sshdPort, "%d", &port); err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager([]deploy.Deployment{{
		Name:      "itest",
		Host:      "127.0.0.1",
		User:      me.Username,
		Port:      port,
		AdminAddr: adminAddr,
	}}, Options{
		Dir:       projDir,
		Token:     token,
		Identity:  clientKey,
		SocketDir: filepath.Join(dir, "t"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	rt, err := mgr.Transport("itest")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://itest/admin/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip through real tunnel: %v\nsshd log:\n%s", err, sshdLog.String())
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "tunneled" {
		t.Fatalf("round trip = %d %q, want 200 tunneled", resp.StatusCode, body)
	}
	if st := mgr.States()["itest"]; st.Status != StatusUp {
		t.Fatalf("state = %s, want up", st.Status)
	}
}

// waitDialable polls until addr accepts a connection.
func waitDialable(t *testing.T, network, addr string, timeout time.Duration, log *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout(network, addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s %s never became dialable; sshd log:\n%s", network, addr, log.String())
}
