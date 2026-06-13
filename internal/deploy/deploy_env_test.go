package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedDeployTime = time.Date(2026, 6, 12, 10, 15, 0, 0, time.UTC)

// fixtureReleaseID is the release ID every deploy in these tests produces:
// the fixture project is not a git checkout, so the short sha is "nogit".
const fixtureReleaseID = "20260612T101500Z-nogit"

// fixtureBinarySHA is the sha256 of the stub go runner's binary content.
var fixtureBinarySHA = func() string {
	sum := sha256.Sum256([]byte("ouvrier-test-binary"))
	return hex.EncodeToString(sum[:])
}()

// baseEnvOpts returns EnvOpts wired to the fake seams: fixed clock, no real
// sleeping, two health attempts, and a throwaway inventory.
func baseEnvOpts(t *testing.T, dir string, remote *fakeRemote) EnvOpts {
	t.Helper()
	return EnvOpts{
		Dir:            dir,
		Hosts:          []string{"h"},
		User:           "deploy",
		Path:           "/opt/ouvrier/demo",
		GoRun:          stubGoRunner(t),
		Runner:         remote,
		Now:            func() time.Time { return fixedDeployTime },
		Sleep:          func(time.Duration) {},
		HealthAttempts: 2,
		InventoryPath:  filepath.Join(t.TempDir(), "deployments.json"),
	}
}

func runDeployEnv(t *testing.T, opts EnvOpts) (*bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := DeployEnvironment(context.Background(), opts, ProgressWriter{Out: &out, Err: &errOut})
	return &out, &errOut, err
}

// assertInOrder asserts every want appears in log, in order.
func assertInOrder(t *testing.T, log string, wants ...string) {
	t.Helper()
	idx := 0
	for _, want := range wants {
		j := strings.Index(log[idx:], want)
		if j < 0 {
			t.Fatalf("sequence missing %q after offset %d in:\n%s", want, idx, log)
		}
		idx += j + len(want)
	}
}

// Acceptance: the full happy-path step sequence — commands, ordering,
// quoting, sha256 verification, lock acquire/release, atomic env install,
// admin-addr injection, prune to --keep — asserted against the fake runner.
func TestDeployEnvHappySequence(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{stdoutFor: map[string]string{
		"readlink": "releases/20260605T000000Z-eeeeeee\n",
		"ls -1": strings.Join([]string{
			"20260601T000000Z-aaaaaaa",
			"20260602T000000Z-bbbbbbb",
			"20260603T000000Z-ccccccc",
			"20260604T000000Z-ddddddd",
			"20260605T000000Z-eeeeeee",
			"20260606T000000Z-fffffff",
			"20260607T000000Z-1111111",
			fixtureReleaseID,
		}, "\n") + "\n",
	}}
	opts := baseEnvOpts(t, dir, remote)
	opts.EnvName = "staging"
	out, errOut, err := runDeployEnv(t, opts)
	if err != nil {
		t.Fatalf("DeployEnvironment() error = %v\nstderr=%s", err, errOut.String())
	}

	const root = "/opt/ouvrier/demo"
	relDir := root + "/releases/" + fixtureReleaseID
	log := remote.callLog()
	assertInOrder(t, log,
		// Step 2: sudo probe, then one preflight ssh (systemd check, user,
		// layout, lock — lock last so failures never leave a stale holder).
		"ssh@h: sudo -n /usr/bin/true",
		"command -v systemctl",
		"sudo /usr/sbin/useradd --system",
		"sudo /usr/bin/install -d -m 0755 -o 'deploy' -- '"+root+"'",
		"mkdir -p -- '"+root+"/releases'",
		"flock -n '"+root+"/.deploy.lock'",
		// Step 3: release skeleton, uploads, sha256 verify + chmod.
		"ssh@h: mkdir -p -- '"+relDir+"/bin' '"+relDir+"/skills'",
		"scp@h: "+relDir+"/bin/demo",
		"scpdata@h: "+relDir+"/RELEASE.json",
		`ssh@h: [ "$(sha256sum -- '`+relDir+`/bin/demo' | cut -d' ' -f1)" = '`+fixtureBinarySHA+`' ] && chmod 0755 -- '`+relDir+"/bin/demo'",
		// Step 4: atomic env install (stage, privileged 0640 promote, rm).
		"scpdata@h: "+root+"/.env.new",
		"sudo /usr/bin/install -o root -g 'ouvrier-demo' -m 0640 -- '"+root+"/.env.new' '"+root+"/shared/.env'",
		"rm -f -- '"+root+"/.env.new'",
		// Step 5: unit install only-if-changed + enable.
		"scpdata@h: "+root+"/ouvrier-demo.service",
		"sudo /usr/bin/install -m 0644 -- '"+root+"/ouvrier-demo.service' '/etc/systemd/system/ouvrier-demo.service'",
		"sudo /usr/bin/systemctl enable 'ouvrier-demo.service'",
		// Step 6: record previous, swap, restart.
		"ssh@h: readlink -- '"+root+"/current'",
		"ln -sfn -- 'releases/"+fixtureReleaseID+"' '"+root+"/current.tmp'",
		"sudo /usr/bin/systemctl restart 'ouvrier-demo.service'",
		// Step 7: health gate over stdin config, never argv.
		"sshin@h: curl -fsS -o /dev/null --max-time 5 -K - 'http://127.0.0.1:9090/admin/health'",
		// Step 8: deploys.log, prune to keep (default 5: 8 entries -> 3 rm),
		// release the lock.
		"previous=releases/20260605T000000Z-eeeeeee' >> '"+root+"/deploys.log'",
		"ssh@h: ls -1 -- '"+root+"/releases'",
		"rm -rf -- '"+root+"/releases/20260601T000000Z-aaaaaaa'",
		"rm -rf -- '"+root+"/releases/20260602T000000Z-bbbbbbb'",
		"rm -rf -- '"+root+"/releases/20260603T000000Z-ccccccc'",
		"ssh@h: : > '"+root+"/.deploy.lock'",
	)
	if strings.Count(log, "rm -rf") != 3 {
		t.Fatalf("prune must delete exactly 3 releases:\n%s", log)
	}

	// The shipped env payload is the operator's file verbatim plus the
	// injected loopback admin addr.
	var envPayload, releaseJSON, unit []byte
	for _, c := range remote.calls {
		switch c.Remote {
		case root + "/.env.new":
			envPayload = c.Data
		case relDir + "/RELEASE.json":
			releaseJSON = c.Data
		case root + "/ouvrier-demo.service":
			unit = c.Data
		}
	}
	localEnv, _ := os.ReadFile(filepath.Join(dir, ".env"))
	want := string(localEnv) + "OUVRIER_ADMIN_ADDR=127.0.0.1:9090\n"
	if string(envPayload) != want {
		t.Fatalf("shipped env payload = %q, want %q", envPayload, want)
	}
	var info ReleaseInfo
	if err := json.Unmarshal(releaseJSON, &info); err != nil {
		t.Fatalf("RELEASE.json invalid: %v", err)
	}
	if info.SHA256 != fixtureBinarySHA {
		t.Fatalf("RELEASE.json sha256 = %q, want %q", info.SHA256, fixtureBinarySHA)
	}
	for _, wantUnit := range []string{
		"ExecStart=/opt/ouvrier/demo/current/bin/demo",
		"User=ouvrier-demo",
		"EnvironmentFile=/opt/ouvrier/demo/shared/.env",
	} {
		if !strings.Contains(string(unit), wantUnit) {
			t.Fatalf("unit missing %q:\n%s", wantUnit, unit)
		}
	}
	if !strings.Contains(out.String(), "deployed demo release "+fixtureReleaseID+" to h") {
		t.Fatalf("missing success line:\n%s", out.String())
	}
}

// Acceptance: deploy refuses to proceed without an ouvrier.known_hosts entry
// for every target — before the build and before any remote command.
func TestDeployEnvFailsFastWhenAnyHostNotTrusted(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	built := false
	opts := baseEnvOpts(t, dir, remote)
	opts.Hosts = []string{"h", "untrusted.example.com"}
	opts.GoRun = func(_ context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
		built = true
		return errors.New("must not build against untrusted host")
	}
	_, _, err := runDeployEnv(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("DeployEnvironment() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "ouvrier server trust untrusted.example.com") {
		t.Fatalf("error = %v, want `ouvrier server trust <host>` hint", err)
	}
	if built {
		t.Fatal("built the binary before the trust gate")
	}
	if len(remote.calls) != 0 {
		t.Fatalf("ran remote commands against untrusted host:\n%s", remote.callLog())
	}
}

// A host pinned only under a different port token must also fail fast: ssh
// would look up "[host]:2222", not "host".
func TestDeployEnvTrustGateUsesPortQualifiedHostname(t *testing.T) {
	dir := writeDeployFixture(t)
	opts := baseEnvOpts(t, dir, &fakeRemote{})
	opts.Port = 2222
	_, _, err := runDeployEnv(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "[h]:2222") || !strings.Contains(err.Error(), "ouvrier server trust h --port 2222") {
		t.Fatalf("error = %v, want port-qualified trust hint", err)
	}

	pinHost(t, dir, "[h]:2222")
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() with pinned [h]:2222 error = %v", err)
	}
}

// A changed host key (ssh's "Host key verification failed") is remapped to a
// hard error naming `ouvrier server trust --rotate`.
func TestDeployEnvRemapsChangedHostKeyToRotate(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{
		failSSHContaining: "sudo -n",
		sshErr:            errors.New("ssh: exit status 255 (stderr=Host key verification failed.)"),
	}
	_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "ouvrier server trust --rotate h") {
		t.Fatalf("error = %v, want `ouvrier server trust --rotate <host>` hint", err)
	}
}

// The runner seam receives the identity file and pinned known_hosts path on
// every ssh and scp invocation; user@host targets are split for ssh.
func TestDeployEnvPlumbsIdentityKnownHostsAndUser(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := baseEnvOpts(t, dir, remote)
	opts.User = ""
	opts.Hosts = []string{"ops@h"}
	opts.Identity = "/keys/ci_ed25519"
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	if remote.lastConnect.Identity != "/keys/ci_ed25519" {
		t.Fatalf("ConnectOpts.Identity = %q, want /keys/ci_ed25519", remote.lastConnect.Identity)
	}
	if remote.lastConnect.KnownHosts != filepath.Join(dir, KnownHostsFile) {
		t.Fatalf("ConnectOpts.KnownHosts = %q, want %s", remote.lastConnect.KnownHosts, filepath.Join(dir, KnownHostsFile))
	}
	if remote.lastConnect.User != "ops" || remote.lastConnect.Host != "h" {
		t.Fatalf("ConnectOpts user/host = %q/%q, want ops/h", remote.lastConnect.User, remote.lastConnect.Host)
	}
}

// An explicit --user wins over the user@ embedded in the host.
func TestDeployEnvUserFlagWinsOverHostUser(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := baseEnvOpts(t, dir, remote)
	opts.User = "ci"
	opts.Hosts = []string{"ops@h"}
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	if remote.lastConnect.User != "ci" {
		t.Fatalf("ConnectOpts.User = %q, want ci", remote.lastConnect.User)
	}
}

func TestDeployEnvRequiresEnvFile(t *testing.T) {
	dir := writeDeployFixture(t)
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	opts := baseEnvOpts(t, dir, &fakeRemote{})
	opts.EnvName = "staging"
	_, _, err := runDeployEnv(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{".env.staging", "--env-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want mention of %q", err, want)
		}
	}
}

// The preflight requires OUVRIER_ADMIN_TOKEN plus pip.yaml env.required, and
// reports missing names only.
func TestDeployEnvValidatesRequiredEnvNames(t *testing.T) {
	dir := writeDeployFixture(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OTHER=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	opts := baseEnvOpts(t, dir, remote)
	opts.EnvRequired = []string{"ANTHROPIC_API_KEY"}
	_, _, err := runDeployEnv(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"OUVRIER_ADMIN_TOKEN", "ANTHROPIC_API_KEY", "missing required keys"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	if len(remote.calls) != 0 {
		t.Fatalf("preflight failure must precede any remote command:\n%s", remote.callLog())
	}
}

// An env file that already pins a loopback admin addr ships verbatim (no
// injected line) and the health gate targets its port.
func TestDeployEnvHonorsLoopbackAdminAddr(t *testing.T) {
	dir := writeDeployFixture(t)
	env := "OUVRIER_ADMIN_TOKEN=" + fixtureAdminToken + "\nOUVRIER_ADMIN_ADDR=127.0.0.1:8088\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	if _, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote)); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	for _, c := range remote.calls {
		if c.Remote == "/opt/ouvrier/demo/.env.new" && string(c.Data) != env {
			t.Fatalf("env payload rewritten: %q", c.Data)
		}
	}
	if !strings.Contains(remote.callLog(), "'http://127.0.0.1:8088/admin/health'") {
		t.Fatalf("health gate must target the admin addr's port:\n%s", remote.callLog())
	}
}

// Acceptance: a public admin target is refused without --allow-shared-admin.
func TestDeployEnvRefusesSharedAdminAddr(t *testing.T) {
	dir := writeDeployFixture(t)
	env := "OUVRIER_ADMIN_TOKEN=" + fixtureAdminToken + "\nOUVRIER_ADMIN_ADDR=0.0.0.0:9090\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "--allow-shared-admin") {
		t.Fatalf("error = %v, want refusal naming --allow-shared-admin", err)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("refusal must happen before any remote command:\n%s", remote.callLog())
	}

	opts := baseEnvOpts(t, dir, remote)
	opts.AllowSharedAdmin = true
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() with --allow-shared-admin error = %v", err)
	}
	if !strings.Contains(remote.callLog(), "'http://127.0.0.1:9090/admin/health'") {
		t.Fatalf("health gate must still probe loopback on the admin port:\n%s", remote.callLog())
	}
}

// Acceptance: a remote sha256 mismatch aborts before the symlink swap and
// still releases the deploy lock.
func TestDeployEnvShaMismatchAbortsBeforeSwap(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{failSSHContaining: "chmod 0755"}
	_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "sha256 verification failed") {
		t.Fatalf("error = %v, want sha256 verification failure", err)
	}
	log := remote.callLog()
	for _, banned := range []string{"ln -sfn", "systemctl restart", "curl"} {
		if strings.Contains(log, banned) {
			t.Fatalf("deploy continued past failed sha256 verify (%q):\n%s", banned, log)
		}
	}
	if !strings.Contains(log, ": > '/opt/ouvrier/demo/.deploy.lock'") {
		t.Fatalf("deploy lock not released on failure:\n%s", log)
	}
	// The lock release must come after the failed verify.
	assertInOrder(t, log, "chmod 0755", ": > '/opt/ouvrier/demo/.deploy.lock'")
}

// Acceptance failure path: health gate failure dumps journalctl, repoints
// current to the recorded previous release, restarts, re-checks, and exits
// nonzero carrying both the cause and the rollback outcome.
func TestDeployEnvHealthFailureRollsBack(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{
		sshInFailures: 2, // both health gate attempts fail; rollback re-check passes
		stdoutFor:     map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"},
	}
	out, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"health gate failed after 2 attempts", "rolled back to 20260605T000000Z-eeeeeee, health OK"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want both cause and rollback outcome (%q)", err, want)
		}
	}
	log := remote.callLog()
	assertInOrder(t, log,
		// New release went live...
		"ln -sfn -- 'releases/"+fixtureReleaseID+"'",
		"sudo /usr/bin/systemctl restart 'ouvrier-demo.service'",
		// ...health gate failed; capture journal, repoint, restart, re-check.
		"sudo /usr/bin/journalctl -u 'ouvrier-demo.service' -n 50 --no-pager",
		"ln -sfn -- 'releases/20260605T000000Z-eeeeeee'",
		"sudo /usr/bin/systemctl restart 'ouvrier-demo.service'",
		"sshin@h: curl",
		// The lock is released even on the failure path.
		": > '/opt/ouvrier/demo/.deploy.lock'",
	)
	// 2 failed gate attempts + 1 successful rollback re-check.
	if got := strings.Count(log, "sshin@h: curl"); got != 3 {
		t.Fatalf("health probe count = %d, want 3:\n%s", got, log)
	}
	if strings.Contains(log, "deploys.log") {
		t.Fatalf("deploys.log must not record a failed deploy:\n%s", log)
	}
	if !strings.Contains(out.String(), "rolling back h to 20260605T000000Z-eeeeeee") {
		t.Fatalf("missing rollback progress line:\n%s", out.String())
	}
}

// A failed rollback reports both errors.
func TestDeployEnvRollbackFailureReportsBothErrors(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{
		failSSHInAll: true, // health gate and rollback re-check both fail
		stdoutFor:    map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"},
	}
	_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"health gate failed", "rollback to 20260605T000000Z-eeeeeee also failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}

// Acceptance: a first deploy (no previous release) degrades to stop+report.
func TestDeployEnvFirstDeployFailureStopsService(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{failSSHInAll: true} // readlink yields nothing: first deploy
	_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "no previous release to roll back to; service stopped") {
		t.Fatalf("error = %v, want first-deploy stop+report", err)
	}
	log := remote.callLog()
	assertInOrder(t, log,
		"sudo /usr/bin/journalctl -u 'ouvrier-demo.service'",
		"sudo /usr/bin/systemctl stop 'ouvrier-demo.service'",
		": > '/opt/ouvrier/demo/.deploy.lock'",
	)
	// Exactly one swap (the failed release); no rollback repoint.
	if got := strings.Count(log, "if mv -T --help"); got != 1 {
		t.Fatalf("swap count = %d, want 1 (no rollback target on first deploy):\n%s", got, log)
	}
}

// Acceptance: multi-host deploys are sequential, abort on the first failure,
// and shout about the resulting mixed-version fleet.
func TestDeployEnvMultiHostAbortsOnFirstFailure(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{
		failHost:     "h2",
		failSSHInAll: true,
		stdoutFor:    map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"},
	}
	opts := baseEnvOpts(t, dir, remote)
	opts.Hosts = []string{"ops@h1", "ops@h2", "ops@h3"}
	opts.User = ""
	_, errOut, err := runDeployEnv(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, c := range remote.calls {
		if c.Host == "h3" {
			t.Fatalf("h3 must not be touched after the h2 failure:\n%s", remote.callLog())
		}
	}
	if !strings.Contains(remote.callLog(), "ssh@h1: : > ") {
		t.Fatalf("h1 deploy did not complete:\n%s", remote.callLog())
	}
	for _, want := range []string{
		"MIXED VERSIONS",
		"aborted after 1 of 3 hosts",
		"ok    ops@h1 (release " + fixtureReleaseID + ")",
		"FAIL  ops@h2",
		"skip  ops@h3 (not attempted)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("abort summary missing %q:\n%s", want, errOut.String())
		}
	}
	// Inventory: h1 recorded, h2/h3 absent.
	inv, invErr := LoadInventory(opts.InventoryPath)
	if invErr != nil {
		t.Fatalf("LoadInventory: %v", invErr)
	}
	if len(inv.Deployments) != 1 || inv.Deployments[0].Host != "h1" {
		t.Fatalf("inventory = %+v, want exactly the h1 entry", inv.Deployments)
	}
}

// Acceptance: the admin token is never in any argv (local or remote command
// string) and never in operator output; it travels only on the SSHIn stdin
// channel (and inside the shipped env file itself).
func TestDeployEnvTokenNeverInArgvOrOutput(t *testing.T) {
	for name, remote := range map[string]*fakeRemote{
		"happy": {stdoutFor: map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"}},
		"health failure": {
			failSSHInAll:       true,
			echoCommandInError: true,
			stdoutFor:          map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeDeployFixture(t)
			out, errOut, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
			if err != nil && !errors.Is(err, ErrDeploy) {
				t.Fatalf("error = %v", err)
			}
			for _, c := range remote.calls {
				if strings.Contains(c.Command, fixtureAdminToken) {
					t.Fatalf("token leaked into a remote command argv: %s", c.Op)
				}
			}
			if strings.Contains(out.String(), fixtureAdminToken) {
				t.Fatalf("token leaked into stdout:\n%s", out.String())
			}
			if strings.Contains(errOut.String(), fixtureAdminToken) {
				t.Fatalf("token leaked into stderr:\n%s", errOut.String())
			}
			if err != nil && strings.Contains(err.Error(), fixtureAdminToken) {
				t.Fatalf("token leaked into the returned error: %v", err)
			}
			// The transport that is allowed to carry it: the curl stdin config.
			found := false
			for _, c := range remote.calls {
				if c.Op == "sshin" && string(c.Data) == `header = "Authorization: Bearer `+fixtureAdminToken+`"`+"\n" {
					found = true
				}
			}
			if !found {
				t.Fatal("health gate did not feed the bearer token via the curl stdin config")
			}
		})
	}
}

// The health gate retries 10 times by default, sleeping between attempts.
func TestDeployEnvHealthGateAttemptsAndSleeps(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{failSSHInAll: true}
	var sleeps []time.Duration
	opts := baseEnvOpts(t, dir, remote)
	opts.HealthAttempts = 0 // default 10
	opts.Sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	_, _, err := runDeployEnv(t, opts)
	if err == nil || !strings.Contains(err.Error(), "after 10 attempts") {
		t.Fatalf("error = %v, want 10-attempt health failure", err)
	}
	probes := 0
	for _, c := range remote.calls {
		if c.Op == "sshin" {
			probes++
		}
	}
	if probes != 10 {
		t.Fatalf("health probes = %d, want 10", probes)
	}
	if len(sleeps) != 9 {
		t.Fatalf("sleeps = %d, want 9 (between attempts only)", len(sleeps))
	}
	for _, d := range sleeps {
		if d != 3*time.Second {
			t.Fatalf("sleep = %v, want 3s", d)
		}
	}
}

// --target is passed through to the cross-compile (e.g. arm64 hosts).
func TestDeployEnvTargetPassthrough(t *testing.T) {
	dir := writeDeployFixture(t)
	var capturedEnv []string
	goRun := func(_ context.Context, _ string, env []string, args []string, _, _ io.Writer) error {
		capturedEnv = env
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-o" {
				_ = os.MkdirAll(filepath.Dir(args[i+1]), 0o755)
				return os.WriteFile(args[i+1], []byte("ouvrier-test-binary"), 0o755)
			}
		}
		return fmt.Errorf("missing -o")
	}
	opts := baseEnvOpts(t, dir, &fakeRemote{})
	opts.GoRun = goRun
	opts.Target = "linux/arm64"
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	want := map[string]bool{"CGO_ENABLED=0": false, "GOOS=linux": false, "GOARCH=arm64": false}
	for _, kv := range capturedEnv {
		if _, ok := want[kv]; ok {
			want[kv] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Fatalf("build env missing %q", k)
		}
	}

	opts.Target = "bogus"
	if _, _, err := runDeployEnv(t, opts); err == nil || !strings.Contains(err.Error(), "GOOS/GOARCH") {
		t.Fatalf("bogus target error = %v, want GOOS/GOARCH validation", err)
	}
}

// Acceptance: a successful host deploy upserts the deployments inventory with
// the documented fields and no secrets.
func TestDeployEnvInventoryUpsert(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := baseEnvOpts(t, dir, remote)
	opts.Port = 2222
	pinHost(t, dir, "[h]:2222")
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	inv, err := LoadInventory(opts.InventoryPath)
	if err != nil {
		t.Fatalf("LoadInventory: %v", err)
	}
	if len(inv.Deployments) != 1 {
		t.Fatalf("inventory entries = %d, want 1", len(inv.Deployments))
	}
	d := inv.Deployments[0]
	want := Deployment{
		Name:       "demo",
		Host:       "h",
		User:       "deploy",
		Port:       2222,
		Path:       "/opt/ouvrier/demo",
		Service:    "ouvrier-demo",
		AdminAddr:  "127.0.0.1:9090",
		HealthPath: "/admin/health",
		SHA256:     fixtureBinarySHA,
		GitRev:     "",
		DeployedAt: fixedDeployTime,
		Result:     "ok",
	}
	if !d.DeployedAt.Equal(want.DeployedAt) {
		t.Fatalf("DeployedAt = %v, want %v", d.DeployedAt, want.DeployedAt)
	}
	d.DeployedAt = want.DeployedAt
	if d != want {
		t.Fatalf("inventory entry = %+v, want %+v", d, want)
	}
	raw, _ := os.ReadFile(opts.InventoryPath)
	if strings.Contains(string(raw), fixtureAdminToken) {
		t.Fatal("inventory must never contain the admin token")
	}
}

// Acceptance: every sudo invocation the flow actually runs is covered by the
// generated sudoers snippet (the flow stays inside its least-privilege grant).
func TestDeployEnvSudoCommandsCoveredBySudoers(t *testing.T) {
	dir := writeDeployFixture(t)
	for name, remote := range map[string]*fakeRemote{
		"happy": {stdoutFor: map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"}},
		"failure": {
			failSSHInAll: true,
			stdoutFor:    map[string]string{"readlink": "releases/20260605T000000Z-eeeeeee\n"},
		},
		"first deploy failure": {failSSHInAll: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
			_ = err // failure paths still must stay within the sudoers grant

			snippet := SudoersSnippet(SudoersParams{
				DeployUser: "deploy", Name: "demo", Service: "ouvrier-demo", Root: "/opt/ouvrier/demo",
			})
			grants := map[string]bool{}
			for _, line := range strings.Split(snippet, "\n") {
				line = strings.TrimSpace(line)
				if after, ok := strings.CutPrefix(line, "deploy ALL=(root) NOPASSWD: "); ok {
					grants[after] = true
				}
			}
			for _, cmd := range remote.sshCommands() {
				for _, part := range splitShellSegments(cmd) {
					k := strings.Index(part, "sudo ")
					if k < 0 {
						continue
					}
					argv := strings.TrimSpace(strings.TrimPrefix(part[k+len("sudo "):], "-n "))
					argv = strings.ReplaceAll(argv, "'", "")
					if !grants[argv] {
						t.Fatalf("flow sudo invocation %q has no sudoers grant; snippet:\n%s", argv, snippet)
					}
				}
			}
		})
	}
}

// When the ssh login user is unknown locally, the flow resolves it remotely
// (`id -un`) before rendering the ownership-sensitive layout commands.
func TestDeployEnvResolvesRemoteOwner(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{stdoutFor: map[string]string{"id -un": "ciops\n"}}
	opts := baseEnvOpts(t, dir, remote)
	opts.User = ""
	opts.Hosts = []string{"h"} // no user@ either
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	log := remote.callLog()
	assertInOrder(t, log, "ssh@h: id -un", "install -d -m 0755 -o 'ciops' -- '/opt/ouvrier/demo'")
}

// skills/ runtime assets ship into the immutable release directory.
func TestDeployEnvUploadsSkillsIntoRelease(t *testing.T) {
	dir := writeDeployFixture(t)
	skillDir := filepath.Join(dir, "skills", "jorf")
	if err := os.MkdirAll(filepath.Join(skillDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: jorf\ndescription: Watch JORF.\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "scripts", "parse.txt"), []byte("asset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := &fakeRemote{}
	if _, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote)); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	relDir := "/opt/ouvrier/demo/releases/" + fixtureReleaseID
	uploads := map[string]bool{}
	for _, c := range remote.calls {
		if c.Op == "scp" {
			uploads[c.Remote] = true
		}
	}
	for _, want := range []string{
		relDir + "/skills/jorf/SKILL.md",
		relDir + "/skills/jorf/scripts/parse.txt",
	} {
		if !uploads[want] {
			t.Fatalf("missing runtime asset upload %s:\n%s", want, remote.callLog())
		}
	}
	if !strings.Contains(remote.callLog(), "mkdir -p -- '"+relDir+"/skills/jorf/scripts'") {
		t.Fatalf("missing skills mkdir:\n%s", remote.callLog())
	}
}

// The unit sandbox toggle reaches the rendered unit (flag wins over pip.yaml).
func TestDeployEnvSandboxToggle(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{}
	opts := baseEnvOpts(t, dir, remote)
	opts.EnvSandbox = "off"
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	var unit string
	for _, c := range remote.calls {
		if c.Remote == "/opt/ouvrier/demo/ouvrier-demo.service" {
			unit = string(c.Data)
		}
	}
	if strings.Contains(unit, "ProtectSystem=strict") {
		t.Fatalf("pip.yaml sandbox: off must drop hardening:\n%s", unit)
	}

	remote = &fakeRemote{}
	opts.Runner = remote
	opts.UnitSandbox = "on" // flag wins
	if _, _, err := runDeployEnv(t, opts); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	for _, c := range remote.calls {
		if c.Remote == "/opt/ouvrier/demo/ouvrier-demo.service" && !strings.Contains(string(c.Data), "ProtectSystem=strict") {
			t.Fatalf("--unit-sandbox on must win over pip.yaml off:\n%s", c.Data)
		}
	}
}

// Carry-over (a): unsafe service names / install roots are rejected before
// any remote command (systemd unit and sudoers injection surface).
func TestDeployEnvRejectsUnsafeServiceAndPath(t *testing.T) {
	dir := writeDeployFixture(t)
	for _, tc := range []struct{ service, path string }{
		{service: "bad name"},
		{service: "bad\nname"},
		{path: "/opt/bad path"},
		{path: "/opt/bad\npath"},
		{path: "relative/path"},
		{service: "bad'quote"},
	} {
		remote := &fakeRemote{}
		opts := baseEnvOpts(t, dir, remote)
		if tc.service != "" {
			opts.Service = tc.service
		}
		if tc.path != "" {
			opts.Path = tc.path
		}
		_, _, err := runDeployEnv(t, opts)
		if !errors.Is(err, ErrDeploy) {
			t.Fatalf("service=%q path=%q: error = %v, want ErrDeploy", tc.service, tc.path, err)
		}
		if len(remote.calls) != 0 {
			t.Fatalf("service=%q path=%q: remote commands ran with unsafe params:\n%s", tc.service, tc.path, remote.callLog())
		}
	}
}

// An actionable error when passwordless sudo is missing, pointing at the
// sudoers snippet generator.
func TestDeployEnvSudoProbeFailureIsActionable(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{failSSHContaining: "sudo -n /usr/bin/true"}
	_, _, err := runDeployEnv(t, baseEnvOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"passwordless sudo", "--print-sudoers", "/etc/sudoers.d/ouvrier-demo"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}
