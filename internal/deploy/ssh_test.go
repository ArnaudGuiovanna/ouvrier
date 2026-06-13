package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// remoteCall is one recorded RemoteRunner invocation, in order.
type remoteCall struct {
	Op      string // "ssh", "sshin", "scp", "scpdata"
	Host    string
	Command string // ssh/sshin remote command
	Local   string // scp local path
	Remote  string // scp/scpdata remote path
	Data    []byte // scpdata payload / sshin stdin
}

// fakeRemote records every SSH/SSHIn/SCP/SCPData invocation in order and can
// inject failures keyed by a substring match on the SSH command, a suffix
// match on the SCP remote path, or (for SSHIn, the health gate) a count of
// failing attempts. When failHost is set, injected failures only apply to
// that host, so multi-host tests can break exactly one target.
type fakeRemote struct {
	mu          sync.Mutex
	calls       []remoteCall
	lastConnect ConnectOpts
	sshInCalls  int

	failSSHContaining   string
	failSCPRemoteSuffix string
	failHost            string
	failSSHInAll        bool
	sshInFailures       int // fail the first N SSHIn calls, then succeed
	// stdoutFor maps an SSH command substring to canned stdout (readlink,
	// ls -1, id -un, ...).
	stdoutFor map[string]string
	// sshErr overrides the default injected SSH failure, so tests can mimic
	// specific ssh stderr shapes (e.g. host key verification failures).
	sshErr error
	// echoCommandInError makes injected failures embed the full remote
	// command in the error, mimicking the shape a runner that echoes its
	// argv would produce.
	echoCommandInError bool
}

func (f *fakeRemote) record(c remoteCall, opts ConnectOpts) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.lastConnect = opts
}

func (f *fakeRemote) failureApplies(opts ConnectOpts) bool {
	return f.failHost == "" || f.failHost == opts.Host
}

func (f *fakeRemote) injectedErr(command string) error {
	if f.sshErr != nil {
		return f.sshErr
	}
	if f.echoCommandInError {
		return fmt.Errorf("ssh -o BatchMode=yes h %s: exit status 22 (stderr=injected)", command)
	}
	return errors.New("ssh injected failure")
}

func (f *fakeRemote) SSH(_ context.Context, opts ConnectOpts, command string) (string, error) {
	f.record(remoteCall{Op: "ssh", Host: opts.Host, Command: command}, opts)
	if f.failSSHContaining != "" && strings.Contains(command, f.failSSHContaining) && f.failureApplies(opts) {
		return "", f.injectedErr(command)
	}
	for sub, out := range f.stdoutFor {
		if strings.Contains(command, sub) {
			return out, nil
		}
	}
	return "", nil
}

func (f *fakeRemote) SSHIn(_ context.Context, opts ConnectOpts, command string, stdin []byte) (string, error) {
	buf := make([]byte, len(stdin))
	copy(buf, stdin)
	f.record(remoteCall{Op: "sshin", Host: opts.Host, Command: command, Data: buf}, opts)
	if !f.failureApplies(opts) {
		return "", nil
	}
	f.mu.Lock()
	f.sshInCalls++
	n := f.sshInCalls
	f.mu.Unlock()
	if f.failSSHInAll || n <= f.sshInFailures {
		return "", f.injectedErr(command)
	}
	return "", nil
}

func (f *fakeRemote) SCP(_ context.Context, opts ConnectOpts, localPath, remotePath string) error {
	f.record(remoteCall{Op: "scp", Host: opts.Host, Local: localPath, Remote: remotePath}, opts)
	if f.failSCPRemoteSuffix != "" && strings.HasSuffix(remotePath, f.failSCPRemoteSuffix) && f.failureApplies(opts) {
		return errors.New("scp injected failure")
	}
	return nil
}

func (f *fakeRemote) SCPData(_ context.Context, opts ConnectOpts, data []byte, remotePath string) error {
	buf := make([]byte, len(data))
	copy(buf, data)
	f.record(remoteCall{Op: "scpdata", Host: opts.Host, Remote: remotePath, Data: buf}, opts)
	if f.failSCPRemoteSuffix != "" && strings.HasSuffix(remotePath, f.failSCPRemoteSuffix) && f.failureApplies(opts) {
		return errors.New("scp injected failure")
	}
	return nil
}

// sshCommands returns every recorded ssh/sshin remote command, in order.
func (f *fakeRemote) sshCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Op == "ssh" || c.Op == "sshin" {
			out = append(out, c.Command)
		}
	}
	return out
}

// callLog renders the ordered operation log ("ssh: <cmd>", "scp: <remote>",
// "scpdata: <remote>", "sshin: <cmd>"), one entry per line, for ordered
// sequence assertions.
func (f *fakeRemote) callLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.calls {
		switch c.Op {
		case "ssh", "sshin":
			fmt.Fprintf(&b, "%s@%s: %s\n", c.Op, c.Host, c.Command)
		default:
			fmt.Fprintf(&b, "%s@%s: %s\n", c.Op, c.Host, c.Remote)
		}
	}
	return b.String()
}

const fixtureAdminToken = "fixture-admin-token"

func writeDeployFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	env := "ANTHROPIC_API_KEY=test\n" + "OUVRIER_ADMIN_TOKEN=" + fixtureAdminToken + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	// Deploys hard-fail against unpinned hosts; pin the hostnames the tests
	// in this package deploy to.
	pinHost(t, dir, "h", "h1", "h2", "h3", "server.example.com")
	return dir
}

// pinHost appends ouvrier.known_hosts entries for the given hostnames, the
// way `ouvrier server trust` would.
func pinHost(t *testing.T, dir string, hosts ...string) {
	t.Helper()
	for _, host := range hosts {
		keys := []HostKey{{Hosts: host, Type: "ssh-ed25519", Key: fixtureEd25519Key}}
		if _, err := UpdateKnownHosts(filepath.Join(dir, KnownHostsFile), host, keys, false); err != nil {
			t.Fatalf("pin host %s: %v", host, err)
		}
	}
}

// stubGoRunner pretends to be `go build` by writing a sentinel byte to the
// expected output binary path. We rely on the same -o flag the real builder
// passes.
func stubGoRunner(t *testing.T) GoRunner {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, args []string, _, _ io.Writer) error {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-o" {
				outPath := args[i+1]
				if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
					return err
				}
				return os.WriteFile(outPath, []byte("ouvrier-test-binary"), 0o755)
			}
		}
		return errors.New("stub go runner: no -o argument")
	}
}

// Acceptance: no password-auth path exists in any ssh/scp invocation — the
// flag assertion on the runner seam's argv builders.
func TestSSHAndSCPArgsHardenAuthentication(t *testing.T) {
	connect := ConnectOpts{
		Host:       "h",
		Port:       2222,
		Identity:   "/keys/ci_ed25519",
		KnownHosts: "/proj/ouvrier.known_hosts",
	}
	for name, args := range map[string][]string{
		"ssh": sshBaseArgs(connect),
		"scp": scpBaseArgs(connect),
	} {
		joined := " " + strings.Join(args, " ") + " "
		for _, want := range []string{
			"-o BatchMode=yes",
			"-o StrictHostKeyChecking=yes",
			"-o PasswordAuthentication=no",
			"-o KbdInteractiveAuthentication=no",
			"-o UserKnownHostsFile=/proj/ouvrier.known_hosts",
			"-i /keys/ci_ed25519",
		} {
			if !strings.Contains(joined, " "+want+" ") {
				t.Fatalf("%s args missing %q: %v", name, want, args)
			}
		}
		for _, banned := range []string{
			"PasswordAuthentication=yes",
			"KbdInteractiveAuthentication=yes",
			"StrictHostKeyChecking=no",
			"StrictHostKeyChecking=accept-new",
			"BatchMode=no",
		} {
			if strings.Contains(joined, banned) {
				t.Fatalf("%s args contain banned option %q: %v", name, banned, args)
			}
		}
	}
	if !strings.Contains(strings.Join(sshBaseArgs(connect), " "), "-p 2222") {
		t.Fatalf("ssh args missing -p 2222: %v", sshBaseArgs(connect))
	}
	if !strings.Contains(strings.Join(scpBaseArgs(connect), " "), "-P 2222") {
		t.Fatalf("scp args missing -P 2222: %v", scpBaseArgs(connect))
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	cases := map[string]string{
		"hello":      "'hello'",
		"a b":        "'a b'",
		"with'quote": `'with'\''quote'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskTokenErrKeepsChainAndMasks(t *testing.T) {
	cause := fmt.Errorf("%w: health gate failed: Authorization: Bearer sekret", ErrDeploy)
	masked := maskTokenErr(cause, "sekret")
	if !errors.Is(masked, ErrDeploy) {
		t.Fatal("maskTokenErr must keep the error chain intact")
	}
	if strings.Contains(masked.Error(), "sekret") {
		t.Fatalf("maskTokenErr leaked the token: %v", masked)
	}
	if got := maskTokenErr(cause, ""); got != cause {
		t.Fatal("empty token must return the error unchanged")
	}
	if got := maskTokenErr(nil, "x"); got != nil {
		t.Fatal("nil error must stay nil")
	}
}

// Sanity check: the test stub runner reports clearly when wired wrong.
func TestStubGoRunnerWritesBinary(t *testing.T) {
	runner := stubGoRunner(t)
	tmp := filepath.Join(t.TempDir(), "bin", "demo")
	err := runner(context.Background(), "", nil, []string{"build", "-o", tmp}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("stub runner error = %v", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read stub output: %v", err)
	}
	if string(data) != "ouvrier-test-binary" {
		t.Fatalf("stub binary content = %q", string(data))
	}
}
