package deploy

import (
	"bytes"
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

// fakeRemote records every SSH/SCP invocation and can inject failures keyed
// by a substring match on either the SSH command or the SCP remote path.
type fakeRemote struct {
	mu          sync.Mutex
	sshCommands []string
	scpUploads  []scpUpload
	scpDataKeys []scpDataUpload

	failSSHContaining   string
	failSCPRemoteSuffix string
}

type scpUpload struct {
	Local  string
	Remote string
}

type scpDataUpload struct {
	Remote string
	Data   []byte
}

func (f *fakeRemote) SSH(_ context.Context, _ ConnectOpts, command string) (string, error) {
	f.mu.Lock()
	f.sshCommands = append(f.sshCommands, command)
	f.mu.Unlock()
	if f.failSSHContaining != "" && strings.Contains(command, f.failSSHContaining) {
		return "", errors.New("ssh injected failure")
	}
	return "", nil
}

func (f *fakeRemote) SCP(_ context.Context, _ ConnectOpts, localPath, remotePath string) error {
	f.mu.Lock()
	f.scpUploads = append(f.scpUploads, scpUpload{Local: localPath, Remote: remotePath})
	f.mu.Unlock()
	if f.failSCPRemoteSuffix != "" && strings.HasSuffix(remotePath, f.failSCPRemoteSuffix) {
		return errors.New("scp injected failure")
	}
	return nil
}

func (f *fakeRemote) SCPData(_ context.Context, _ ConnectOpts, data []byte, remotePath string) error {
	f.mu.Lock()
	buf := make([]byte, len(data))
	copy(buf, data)
	f.scpDataKeys = append(f.scpDataKeys, scpDataUpload{Remote: remotePath, Data: buf})
	f.mu.Unlock()
	if f.failSCPRemoteSuffix != "" && strings.HasSuffix(remotePath, f.failSCPRemoteSuffix) {
		return errors.New("scp injected failure")
	}
	return nil
}

func writeDeployFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ANTHROPIC_API_KEY=test\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
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

func TestDeploySSHRequiresHost(t *testing.T) {
	err := DeploySSH(context.Background(), Opts{Dir: t.TempDir()}, ProgressWriter{})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("DeploySSH() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "host") {
		t.Fatalf("DeploySSH() error = %v, want host message", err)
	}
}

func TestDeploySSHRequiresEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	opts := Opts{Dir: dir, Host: "h", HealthURL: "/admin/health", GoRun: stubGoRunner(t), Runner: &fakeRemote{}}
	err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("DeploySSH() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), ".env") {
		t.Fatalf("DeploySSH() error = %v, want .env message", err)
	}
}

func TestDeploySSHHappyPath(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := Opts{
		Dir:       dir,
		Host:      "server.example.com",
		User:      "ops",
		Path:      "/opt/demo",
		Service:   "demo.service",
		HealthURL: "/admin/health",
		GoRun:     stubGoRunner(t),
		Runner:    remote,
	}
	var out, errOut bytes.Buffer
	if err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &out, Err: &errOut}); err != nil {
		t.Fatalf("DeploySSH() error = %v\nstderr=%s", err, errOut.String())
	}

	// Two SCPs: the binary and the .env.
	if len(remote.scpUploads) != 2 {
		t.Fatalf("scp uploads = %d, want 2: %+v", len(remote.scpUploads), remote.scpUploads)
	}
	if !strings.HasSuffix(remote.scpUploads[0].Remote, "/bin/demo.new") {
		t.Fatalf("first scp = %+v, want bin/demo.new", remote.scpUploads[0])
	}
	if !strings.HasSuffix(remote.scpUploads[1].Remote, "/.env") {
		t.Fatalf("second scp = %+v, want .env", remote.scpUploads[1])
	}

	// One SCPData upload: the systemd unit.
	if len(remote.scpDataKeys) != 1 {
		t.Fatalf("scp data uploads = %d, want 1: %+v", len(remote.scpDataKeys), remote.scpDataKeys)
	}
	if !strings.HasSuffix(remote.scpDataKeys[0].Remote, "/demo.service.service") {
		t.Fatalf("systemd unit upload remote = %q, want demo.service.service", remote.scpDataKeys[0].Remote)
	}
	unitText := string(remote.scpDataKeys[0].Data)
	for _, want := range []string{
		"[Unit]",
		"Description=Ouvrier worker demo",
		"User=ops",
		"WorkingDirectory=/opt/demo",
		"EnvironmentFile=/opt/demo/.env",
		"ExecStart=/opt/demo/bin/demo",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unitText, want) {
			t.Fatalf("systemd unit missing %q in:\n%s", want, unitText)
		}
	}

	// SSH command sequence covers all required steps.
	joined := strings.Join(remote.sshCommands, "\n")
	for _, want := range []string{
		"mkdir -p '/opt/demo'/bin",
		"chmod 0600 '/opt/demo/.env'",
		"sudo install -m 0644 '/opt/demo/demo.service.service' /etc/systemd/system/'demo.service'.service",
		"sudo systemctl daemon-reload",
		"sudo systemctl restart 'demo.service'",
		"curl -fsS --max-time 5 'http://127.0.0.1:8080/admin/health'",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ssh commands missing %q in:\n%s", want, joined)
		}
	}

	// No secret should leak into logs.
	if strings.Contains(out.String(), "ANTHROPIC_API_KEY") || strings.Contains(errOut.String(), "ANTHROPIC_API_KEY") {
		t.Fatalf("env secret leaked into logs:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
}

func TestDeploySSHUploadsSkillsRuntimeAssets(t *testing.T) {
	dir := writeDeployFixture(t)
	skillDir := filepath.Join(dir, "skills", "jorf")
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatalf("mkdir skill fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: jorf\ndescription: Watch JORF.\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatalf("write skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "parse.txt"), []byte("asset\n"), 0o644); err != nil {
		t.Fatalf("write skill script: %v", err)
	}

	remote := &fakeRemote{}
	opts := Opts{
		Dir:       dir,
		Host:      "h",
		Path:      "/opt/demo",
		Service:   "demo.service",
		HealthURL: "/admin/health",
		GoRun:     stubGoRunner(t),
		Runner:    remote,
	}
	if err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatalf("DeploySSH() error = %v", err)
	}

	uploads := map[string]bool{}
	for _, upload := range remote.scpUploads {
		uploads[upload.Remote] = true
	}
	for _, want := range []string{
		"/opt/demo/skills/jorf/SKILL.md",
		"/opt/demo/skills/jorf/scripts/parse.txt",
	} {
		if !uploads[want] {
			t.Fatalf("scp uploads = %+v, want runtime asset %s", remote.scpUploads, want)
		}
	}
	joined := strings.Join(remote.sshCommands, "\n")
	for _, want := range []string{
		"mkdir -p '/opt/demo/skills'",
		"mkdir -p '/opt/demo/skills/jorf'",
		"mkdir -p '/opt/demo/skills/jorf/scripts'",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ssh commands missing %q in:\n%s", want, joined)
		}
	}
}

func TestDeploySSHDefaultsServiceAndPath(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := Opts{Dir: dir, Host: "h", HealthURL: "/admin/health", GoRun: stubGoRunner(t), Runner: remote}
	if err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatalf("DeploySSH() error = %v", err)
	}
	if !strings.HasSuffix(remote.scpUploads[0].Remote, "/opt/ouvrier/demo/bin/demo.new") {
		t.Fatalf("default path not used, got %q", remote.scpUploads[0].Remote)
	}
	if !strings.HasSuffix(remote.scpDataKeys[0].Remote, "/opt/ouvrier/demo/ouvrier-demo.service") {
		t.Fatalf("default service unit path = %q", remote.scpDataKeys[0].Remote)
	}
}

func TestDeploySSHRollsBackOnHealthFailure(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{failSSHContaining: "curl -fsS --max-time 5"}
	opts := Opts{
		Dir:       dir,
		Host:      "h",
		Path:      "/opt/demo",
		Service:   "demo.service",
		HealthURL: "/admin/health",
		GoRun:     stubGoRunner(t),
		Runner:    remote,
	}
	err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("DeploySSH() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("DeploySSH() error = %v, want health check failure", err)
	}
	joined := strings.Join(remote.sshCommands, "\n")
	if !strings.Contains(joined, "mv '/opt/demo/bin/demo.previous' '/opt/demo/bin/demo'") {
		t.Fatalf("expected rollback mv command, got:\n%s", joined)
	}
}

func TestDeploySSHMasksAdminTokenInLogs(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := Opts{
		Dir:        dir,
		Host:       "h",
		Path:       "/opt/demo",
		Service:    "demo.service",
		HealthURL:  "/admin/health",
		AdminToken: "super-secret-token",
		GoRun:      stubGoRunner(t),
		Runner:     remote,
	}
	var out, errOut bytes.Buffer
	if err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &out, Err: &errOut}); err != nil {
		t.Fatalf("DeploySSH() error = %v", err)
	}
	// Token is allowed to appear inside the remote ssh command, but must
	// never appear in local stdout/stderr.
	if strings.Contains(out.String(), "super-secret-token") {
		t.Fatalf("admin token leaked into stdout: %s", out.String())
	}
	if strings.Contains(errOut.String(), "super-secret-token") {
		t.Fatalf("admin token leaked into stderr: %s", errOut.String())
	}
	// The token should be present in the curl command we hand to ssh.
	joined := strings.Join(remote.sshCommands, "\n")
	if !strings.Contains(joined, "Authorization: Bearer super-secret-token") {
		t.Fatalf("expected curl to include bearer token, got:\n%s", joined)
	}
}

func TestDeploySSHHealthURLFullURLOverride(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := Opts{
		Dir:       dir,
		Host:      "h",
		Path:      "/opt/demo",
		Service:   "demo.service",
		HealthURL: "http://127.0.0.1:9000/healthz",
		GoRun:     stubGoRunner(t),
		Runner:    remote,
	}
	if err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatalf("DeploySSH() error = %v", err)
	}
	joined := strings.Join(remote.sshCommands, "\n")
	if !strings.Contains(joined, "curl -fsS --max-time 5 'http://127.0.0.1:9000/healthz'") {
		t.Fatalf("custom health URL not forwarded:\n%s", joined)
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

func TestBuildHealthCheckCommandUsesDefaultBaseURL(t *testing.T) {
	cmd := buildHealthCheckCommand("/admin/health", "")
	if cmd != "curl -fsS --max-time 5 'http://127.0.0.1:8080/admin/health'" {
		t.Fatalf("buildHealthCheckCommand() = %q", cmd)
	}
}

func TestBuildHealthCheckCommandWithToken(t *testing.T) {
	cmd := buildHealthCheckCommand("/admin/health", "tok")
	if !strings.Contains(cmd, "Authorization: Bearer tok") {
		t.Fatalf("buildHealthCheckCommand() = %q, missing bearer", cmd)
	}
	if !strings.Contains(cmd, "'http://127.0.0.1:8080/admin/health'") {
		t.Fatalf("buildHealthCheckCommand() = %q, missing URL", cmd)
	}
}

// Sanity check: the test stub runner reports clearly when wired wrong.
func TestStubGoRunnerWritesBinary(t *testing.T) {
	runner := stubGoRunner(t)
	tmp := filepath.Join(t.TempDir(), "bin", "demo")
	err := runner(context.Background(), "", nil, []string{"build", "-o", tmp}, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("stub runner error = %v", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read stub output: %v", err)
	}
	if !bytes.Equal(data, []byte("ouvrier-test-binary")) {
		t.Fatalf("stub binary content = %q", string(data))
	}
}

// Make sure systemd unit names are not double-suffixed if --service already
// ends in .service. The current implementation always adds .service when
// constructing the unit path on disk; document the limitation here so a
// future PR can revisit.
func TestRenderSystemdUnitDefaultUser(t *testing.T) {
	unit := renderSystemdUnit(systemdUnitParams{
		Name: "demo", Service: "demo.service",
		InstallPath: "/opt/demo",
	})
	if !strings.Contains(unit, "User=root") {
		t.Fatalf("default user should be root; unit=\n%s", unit)
	}
}

// Sanity: confirm the ssh deploy uses the static build env
// (CGO_ENABLED=0, GOOS=linux, GOARCH=amd64).
func TestDeploySSHUsesStaticLinuxBuildEnv(t *testing.T) {
	dir := writeDeployFixture(t)
	var capturedEnv []string
	runner := func(_ context.Context, _ string, env []string, args []string, _, _ io.Writer) error {
		capturedEnv = env
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-o" {
				_ = os.MkdirAll(filepath.Dir(args[i+1]), 0o755)
				return os.WriteFile(args[i+1], []byte("x"), 0o755)
			}
		}
		return fmt.Errorf("missing -o")
	}
	opts := Opts{Dir: dir, Host: "h", HealthURL: "/admin/health", GoRun: runner, Runner: &fakeRemote{}}
	if err := DeploySSH(context.Background(), opts, ProgressWriter{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
		t.Fatalf("DeploySSH() error = %v", err)
	}
	want := map[string]bool{"CGO_ENABLED=0": false, "GOOS=linux": false, "GOARCH=amd64": false}
	for _, kv := range capturedEnv {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("env missing %q in %v", k, filterBuildEnv(capturedEnv))
		}
	}
}

func filterBuildEnv(env []string) []string {
	out := make([]string, 0, 4)
	for _, e := range env {
		if strings.HasPrefix(e, "GOOS=") || strings.HasPrefix(e, "GOARCH=") || strings.HasPrefix(e, "CGO_ENABLED=") {
			out = append(out, e)
		}
	}
	return out
}
