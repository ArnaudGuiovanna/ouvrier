package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseIDFormat(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 15, 0, 0, time.UTC)
	cases := map[string]string{
		"0123456789abcdef0123456789abcdef01234567": "20260612T101500Z-0123456789ab",
		"ABCDEF0":   "20260612T101500Z-abcdef0",
		"":          "20260612T101500Z-nogit",
		"not-hex!":  "20260612T101500Z-nogit",
		"deadbeef ": "20260612T101500Z-deadbeef",
	}
	for sha, want := range cases {
		if got := ReleaseID(now, sha); got != want {
			t.Fatalf("ReleaseID(%q) = %q, want %q", sha, got, want)
		}
	}
	// Non-UTC input must still render the UTC timestamp.
	paris := time.FixedZone("CEST", 2*3600)
	if got := ReleaseID(time.Date(2026, 6, 12, 12, 15, 0, 0, paris), "abc"); got != "20260612T101500Z-abc" {
		t.Fatalf("ReleaseID(non-UTC) = %q, want UTC timestamp", got)
	}
}

// Lexicographic order of release IDs equals chronological order: the prune
// helper relies on it.
func TestReleaseIDSortsChronologically(t *testing.T) {
	older := ReleaseID(time.Date(2026, 6, 12, 23, 59, 59, 0, time.UTC), "fff")
	newer := ReleaseID(time.Date(2026, 6, 13, 0, 0, 0, 0, time.UTC), "aaa")
	if !(older < newer) {
		t.Fatalf("release IDs must sort chronologically: %q !< %q", older, newer)
	}
}

func TestNewReleaseInfoOutsideGit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "demo")
	content := []byte("ouvrier-test-binary")
	if err := os.WriteFile(bin, content, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	now := time.Date(2026, 6, 12, 10, 15, 0, 0, time.UTC)
	info, err := NewReleaseInfo(context.Background(), dir, bin, now)
	if err != nil {
		t.Fatalf("NewReleaseInfo() error = %v", err)
	}
	sum := sha256.Sum256(content)
	if info.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %q, want %q", info.SHA256, hex.EncodeToString(sum[:]))
	}
	if info.GitSHA != "" {
		t.Fatalf("GitSHA = %q, want empty outside a git checkout", info.GitSHA)
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Fatalf("GoVersion = %q, want go toolchain version", info.GoVersion)
	}
	if !strings.Contains(info.Builder, "@") {
		t.Fatalf("Builder = %q, want user@host", info.Builder)
	}
	if info.DeployTime != "2026-06-12T10:15:00Z" {
		t.Fatalf("DeployTime = %q, want UTC RFC3339", info.DeployTime)
	}
}

func TestNewReleaseInfoInsideGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	bin := filepath.Join(dir, "demo")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "init")

	info, err := NewReleaseInfo(context.Background(), dir, bin, time.Now())
	if err != nil {
		t.Fatalf("NewReleaseInfo() error = %v", err)
	}
	if len(info.GitSHA) != 40 {
		t.Fatalf("GitSHA = %q, want full 40-char sha", info.GitSHA)
	}
	if id := ReleaseID(time.Now(), info.GitSHA); strings.HasSuffix(id, "-nogit") {
		t.Fatalf("ReleaseID from GitSHA fell back to nogit: %q", id)
	}
}

func TestNewReleaseInfoMissingBinary(t *testing.T) {
	_, err := NewReleaseInfo(context.Background(), t.TempDir(), "/does/not/exist", time.Now())
	if err == nil || !strings.Contains(err.Error(), "hash release binary") {
		t.Fatalf("NewReleaseInfo() error = %v, want hashing failure", err)
	}
}

func TestReleaseInfoJSONFields(t *testing.T) {
	info := ReleaseInfo{
		SHA256:     "ab",
		GitSHA:     "cd",
		GoVersion:  "go1.24.1",
		Builder:    "ops@build",
		DeployTime: "2026-06-12T10:15:00Z",
	}
	data, err := info.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("JSON() must end with a newline")
	}
	var decoded map[string]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("JSON() output invalid: %v", err)
	}
	for _, key := range []string{"sha256", "git_sha", "go_version", "builder", "deploy_time"} {
		if decoded[key] == "" {
			t.Fatalf("RELEASE.json missing %q: %s", key, data)
		}
	}
}

// Acceptance: the symlink-swap, prune, and lock command sequences are
// asserted against a fake remoteRunner — paths quoted, no unquoted globs.
func TestReleaseCommandSequenceAgainstFakeRunner(t *testing.T) {
	const (
		root  = "/opt/ouvrier/demo"
		name  = "demo"
		owner = "deploy"
	)
	releaseID := ReleaseID(time.Date(2026, 6, 12, 10, 15, 0, 0, time.UTC), "abcdef1234567890")
	now := time.Date(2026, 6, 12, 10, 16, 0, 0, time.UTC)

	// The #45 flow feeds these to RemoteRunner.SSH in this order; the ls
	// output below stands in for the stdout of ListReleasesCommand.
	lsOutput := strings.Join([]string{
		"20260610T000000Z-aaaaaaa",
		"20260611T000000Z-bbbbbbb",
		releaseID,
	}, "\n")
	var commands []string
	commands = append(commands, AcquireLockCommand(root, "deploy@ci pid 42 release "+releaseID))
	commands = append(commands, CreateServiceUserCommand(root, name))
	commands = append(commands, MkdirLayoutCommands(root, name, owner)...)
	commands = append(commands, InstallEnvCommands(root, name)...)
	commands = append(commands, ReadCurrentTargetCommand(root))
	commands = append(commands, SwapCurrentCommands(root, releaseID)...)
	commands = append(commands, AppendDeployLogCommand(root, releaseID, "releases/20260611T000000Z-bbbbbbb", now))
	commands = append(commands, ListReleasesCommand(root))
	commands = append(commands, PruneReleasesCommands(root, lsOutput, 2)...)
	commands = append(commands, ReleaseLockCommand(root))

	remote := &fakeRemote{}
	connect := ConnectOpts{Host: "h", User: owner}
	for _, cmd := range commands {
		if _, err := remote.SSH(context.Background(), connect, cmd); err != nil {
			t.Fatalf("fake runner rejected %q: %v", cmd, err)
		}
	}

	joined := strings.Join(remote.sshCommands(), "\n")
	// Every reference to the install root must be quoted: the bare root may
	// only ever appear immediately after a quote character.
	for _, line := range remote.sshCommands() {
		for i := 0; ; {
			j := strings.Index(line[i:], root)
			if j < 0 {
				break
			}
			at := i + j
			if at == 0 || line[at-1] != '\'' {
				t.Fatalf("unquoted remote path in %q", line)
			}
			i = at + len(root)
		}
	}
	// No globbing, no head|xargs deletion pipelines, ever.
	for _, banned := range []string{"*", "xargs", "head -", "head |"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("command sequence contains banned pattern %q:\n%s", banned, joined)
		}
	}

	// Spot-check the key steps the fake runner received, in order.
	wantInOrder := []string{
		"flock -n '/opt/ouvrier/demo/.deploy.lock'",
		"useradd",
		"mkdir -p -- '/opt/ouvrier/demo/releases'",
		"install -o root -g 'ouvrier-demo' -m 0640 -- '/opt/ouvrier/demo/.env.new' '/opt/ouvrier/demo/shared/.env'",
		"readlink -- '/opt/ouvrier/demo/current'",
		"ln -sfn -- 'releases/" + releaseID + "' '/opt/ouvrier/demo/current.tmp' && mv -T -- '/opt/ouvrier/demo/current.tmp' '/opt/ouvrier/demo/current'",
		">> '/opt/ouvrier/demo/deploys.log'",
		"ls -1 -- '/opt/ouvrier/demo/releases'",
		"rm -rf -- '/opt/ouvrier/demo/releases/20260610T000000Z-aaaaaaa'",
		": > '/opt/ouvrier/demo/.deploy.lock'",
	}
	idx := 0
	for _, want := range wantInOrder {
		rest := joined[idx:]
		j := strings.Index(rest, want)
		if j < 0 {
			t.Fatalf("command sequence missing %q after offset %d:\n%s", want, idx, joined)
		}
		idx += j + len(want)
	}
}

func TestSwapCurrentCommandsPreflightAndFallback(t *testing.T) {
	cmds := SwapCurrentCommands("/opt/ouvrier/demo", "20260612T101500Z-abc")
	if len(cmds) != 1 {
		t.Fatalf("SwapCurrentCommands() = %d commands, want 1", len(cmds))
	}
	cmd := cmds[0]
	for _, want := range []string{
		// GNU mv -T preflight...
		"if mv -T --help >/dev/null 2>&1; then ",
		// ...atomic two-step swap...
		"ln -sfn -- 'releases/20260612T101500Z-abc' '/opt/ouvrier/demo/current.tmp' && mv -T -- '/opt/ouvrier/demo/current.tmp' '/opt/ouvrier/demo/current'",
		// ...documented ln -sfn fallback for non-GNU userlands.
		"else ln -sfn -- 'releases/20260612T101500Z-abc' '/opt/ouvrier/demo/current'; fi",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("SwapCurrentCommands missing %q:\n%s", want, cmd)
		}
	}
	// The symlink target is relative, so the layout survives a root move.
	if strings.Contains(cmd, "'/opt/ouvrier/demo/releases/") {
		t.Fatalf("symlink target must be relative:\n%s", cmd)
	}
}

func TestAppendDeployLogCommand(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 16, 0, 0, time.UTC)
	cmd := AppendDeployLogCommand("/opt/x", "20260612T101500Z-abc", "releases/20260611T000000Z-old", now)
	want := "printf '%s\\n' '2026-06-12T10:16:00Z 20260612T101500Z-abc previous=releases/20260611T000000Z-old' >> '/opt/x/deploys.log'"
	if cmd != want {
		t.Fatalf("AppendDeployLogCommand = %q, want %q", cmd, want)
	}
	// First deploy: no previous current target.
	first := AppendDeployLogCommand("/opt/x", "20260612T101500Z-abc", "", now)
	if !strings.Contains(first, "previous=-'") {
		t.Fatalf("first-deploy log line must record previous=-: %q", first)
	}
}

func TestAcquireLockCommandDiagnosticsAndQuoting(t *testing.T) {
	cmd := AcquireLockCommand("/opt/ouvrier/my app", "ops@ci pid 7")
	if !strings.HasPrefix(cmd, "flock -n '/opt/ouvrier/my app/.deploy.lock' -c ") {
		t.Fatalf("AcquireLockCommand must flock -n the quoted lock path: %q", cmd)
	}
	for _, want := range []string{
		"held by:",     // losing deploys see who holds the lock
		"exit 1",       // and fail
		"ops@ci pid 7", // winners record their identity
		"deploy lock",  // diagnostic names the lock
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("AcquireLockCommand missing %q:\n%s", want, cmd)
		}
	}
	release := ReleaseLockCommand("/opt/ouvrier/my app")
	if release != ": > '/opt/ouvrier/my app/.deploy.lock'" {
		t.Fatalf("ReleaseLockCommand = %q", release)
	}
}

func TestPruneReleasesCommands(t *testing.T) {
	root := "/opt/ouvrier/demo"
	ids := []string{
		"20260601T000000Z-aaaaaaa",
		"20260602T000000Z-bbbbbbb",
		"20260603T000000Z-ccccccc",
		"20260604T000000Z-ddddddd",
		"20260605T000000Z-eeeeeee",
		"20260606T000000Z-fffffff",
		"20260607T000000Z-nogit",
	}
	// ls output arrives unsorted and with entries the deploy never created;
	// those must never be deleted.
	lines := []string{
		ids[3], ids[0], "current.tmp", ids[6], "", ids[1],
		"evil; rm -rf /", ids[5], "20260608T000000Z-NOTHEX", ids[2], ids[4], "  ",
	}
	cmds := PruneReleasesCommands(root, strings.Join(lines, "\n"), 0) // default keep 5
	want := []string{
		"rm -rf -- '/opt/ouvrier/demo/releases/20260601T000000Z-aaaaaaa'",
		"rm -rf -- '/opt/ouvrier/demo/releases/20260602T000000Z-bbbbbbb'",
	}
	if len(cmds) != len(want) {
		t.Fatalf("PruneReleasesCommands = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Fatalf("PruneReleasesCommands[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}

	if got := PruneReleasesCommands(root, strings.Join(ids, "\n"), 10); got != nil {
		t.Fatalf("keep >= count must prune nothing, got %v", got)
	}
	if got := PruneReleasesCommands(root, "", 1); got != nil {
		t.Fatalf("empty ls output must prune nothing, got %v", got)
	}
	if got := PruneReleasesCommands(root, strings.Join(ids, "\n"), 1); len(got) != 6 {
		t.Fatalf("keep 1 must prune 6, got %v", got)
	}
}

func TestMkdirLayoutCommandsOwnershipAndQuoting(t *testing.T) {
	cmds := MkdirLayoutCommands("/opt/ouvrier/demo", "demo", "deploy")
	want := []string{
		"sudo /usr/bin/install -d -m 0755 -o 'deploy' -- '/opt/ouvrier/demo'",
		"mkdir -p -- '/opt/ouvrier/demo/releases'",
		"sudo /usr/bin/install -d -m 0750 -o root -g 'ouvrier-demo' -- '/opt/ouvrier/demo/shared'",
		"sudo /usr/bin/install -d -m 0750 -o 'ouvrier-demo' -g 'ouvrier-demo' -- '/opt/ouvrier/demo/shared/state'",
	}
	if len(cmds) != len(want) {
		t.Fatalf("MkdirLayoutCommands = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Fatalf("MkdirLayoutCommands[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}
}

// Carry-over (b): scp cannot mkdir, so the per-release skeleton helper must
// create releases/<id>/bin and releases/<id>/skills, quoted, without sudo.
func TestMkdirReleaseCommands(t *testing.T) {
	cmds := MkdirReleaseCommands("/opt/ouvrier/demo", "20260612T101500Z-abc")
	want := []string{
		"mkdir -p -- '/opt/ouvrier/demo/releases/20260612T101500Z-abc/bin' '/opt/ouvrier/demo/releases/20260612T101500Z-abc/skills'",
	}
	if len(cmds) != len(want) {
		t.Fatalf("MkdirReleaseCommands = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Fatalf("MkdirReleaseCommands[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}
	for _, cmd := range cmds {
		if strings.Contains(cmd, "sudo") {
			t.Fatalf("release skeleton must not need sudo: %q", cmd)
		}
	}
}

func TestVerifyReleaseBinaryCommand(t *testing.T) {
	cmd := VerifyReleaseBinaryCommand("/opt/x/releases/r/bin/demo", "abc123")
	want := `[ "$(sha256sum -- '/opt/x/releases/r/bin/demo' | cut -d' ' -f1)" = 'abc123' ] && chmod 0755 -- '/opt/x/releases/r/bin/demo'`
	if cmd != want {
		t.Fatalf("VerifyReleaseBinaryCommand = %q, want %q", cmd, want)
	}
}

func TestCreateServiceUserCommandIsIdempotentAndNologin(t *testing.T) {
	cmd := CreateServiceUserCommand("/opt/ouvrier/demo", "demo")
	for _, want := range []string{
		"id -u 'ouvrier-demo' >/dev/null 2>&1 || ",
		"sudo /usr/sbin/useradd --system",
		"--home-dir '/opt/ouvrier/demo'",
		"--no-create-home",
		"--shell /usr/sbin/nologin 'ouvrier-demo'",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("CreateServiceUserCommand missing %q:\n%s", want, cmd)
		}
	}
}

func TestInstallEnvCommands(t *testing.T) {
	cmds := InstallEnvCommands("/opt/ouvrier/demo", "demo")
	want := []string{
		"sudo /usr/bin/install -o root -g 'ouvrier-demo' -m 0640 -- '/opt/ouvrier/demo/.env.new' '/opt/ouvrier/demo/shared/.env'",
		"rm -f -- '/opt/ouvrier/demo/.env.new'",
	}
	if len(cmds) != len(want) {
		t.Fatalf("InstallEnvCommands = %v, want %v", cmds, want)
	}
	for i := range want {
		if cmds[i] != want[i] {
			t.Fatalf("InstallEnvCommands[%d] = %q, want %q", i, cmds[i], want[i])
		}
	}
	if EnvStagePath("/opt/x") != "/opt/x/.env.new" {
		t.Fatalf("EnvStagePath = %q", EnvStagePath("/opt/x"))
	}
}
