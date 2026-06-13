package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ledger fixtures: the host's last deploys.log entry says the live release is
// fixtureReleaseID and it replaced fixturePrevReleaseID.
const (
	fixturePrevReleaseID = "20260605T000000Z-eeeeeee"
	fixtureLedgerLine    = "2026-06-12T10:15:00Z " + fixtureReleaseID + " previous=releases/" + fixturePrevReleaseID
)

// rollbackRemote returns a fakeRemote seeded with a plausible host state: a
// last ledger entry pointing at a previous release, and a current symlink at
// the release that entry activated.
func rollbackRemote() *fakeRemote {
	return &fakeRemote{stdoutFor: map[string]string{
		"tail -n 1": fixtureLedgerLine + "\n",
		"readlink":  "releases/" + fixtureReleaseID + "\n",
	}}
}

// baseRollbackOpts mirrors baseEnvOpts for the rollback flow: fixed clock, no
// real sleeping, two health attempts, throwaway inventory, no build seam.
func baseRollbackOpts(t *testing.T, dir string, remote *fakeRemote) RollbackOpts {
	t.Helper()
	return RollbackOpts{
		Dir:            dir,
		Hosts:          []string{"h"},
		User:           "deploy",
		Path:           "/opt/ouvrier/demo",
		Runner:         remote,
		Now:            func() time.Time { return fixedDeployTime },
		Sleep:          func(time.Duration) {},
		HealthAttempts: 2,
		InventoryPath:  filepath.Join(t.TempDir(), "deployments.json"),
	}
}

func runRollback(t *testing.T, opts RollbackOpts) (*bytes.Buffer, *bytes.Buffer, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := RollbackEnvironment(context.Background(), opts, ProgressWriter{Out: &out, Err: &errOut})
	return &out, &errOut, err
}

// Acceptance: the happy rollback sequence — ledger read, target existence
// check, current read, swap to the EXACT previous target from the ledger,
// restart, health gate, rollback ledger entry, lock release — in order, with
// no build, upload, or layout provisioning.
func TestRollbackHappySequence(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := rollbackRemote()
	out, errOut, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if err != nil {
		t.Fatalf("RollbackEnvironment() error = %v\nstderr=%s", err, errOut.String())
	}

	const root = "/opt/ouvrier/demo"
	log := remote.callLog()
	assertInOrder(t, log,
		// Sudo probe, then the same deploy lock (rollback mutates current).
		"ssh@h: sudo -n /usr/bin/true",
		"flock -n '"+root+"/.deploy.lock'",
		// Target resolution: last ledger entry, never timestamp sorting.
		"ssh@h: tail -n 1 -- '"+root+"/deploys.log'",
		// The previous release must still exist BEFORE current is touched.
		"ssh@h: test -d '"+root+"/releases/"+fixturePrevReleaseID+"'",
		// Record the replaced target for the new ledger entry.
		"ssh@h: readlink -- '"+root+"/current'",
		// Swap to the exact recorded previous target, restart, health gate.
		"ln -sfn -- 'releases/"+fixturePrevReleaseID+"' '"+root+"/current.tmp'",
		"sudo /usr/bin/systemctl restart 'ouvrier-demo.service'",
		"sshin@h: curl -fsS -o /dev/null --max-time 5 -K - 'http://127.0.0.1:9090/admin/health'",
		// Distinguishable rollback ledger entry, then the lock release.
		"printf '%s\\n' '2026-06-12T10:15:00Z "+fixturePrevReleaseID+" previous=releases/"+fixtureReleaseID+" rollback' >> '"+root+"/deploys.log'",
		"ssh@h: : > '"+root+"/.deploy.lock'",
	)
	// No build artifacts move and nothing is provisioned: rollback only
	// repoints current.
	for _, banned := range []string{"scp@", "scpdata@", "useradd", "install -", "mkdir", ".env"} {
		if strings.Contains(log, banned) {
			t.Fatalf("rollback must not run %q:\n%s", banned, log)
		}
	}
	if !strings.Contains(out.String(), "rolled back demo on h to "+fixturePrevReleaseID) {
		t.Fatalf("missing success line:\n%s", out.String())
	}
}

// Acceptance: the rollback target is the exact previous= target of the last
// ledger entry, even when it is not the lexicographically previous release.
func TestRollbackUsesExactLedgerPrevious(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{stdoutFor: map[string]string{
		// The ledger says the live release replaced an OLD release (e.g. the
		// in-between releases failed their health gates and never landed).
		"tail -n 1": "2026-06-12T10:15:00Z " + fixtureReleaseID + " previous=releases/20260101T000000Z-abcdef123456\n",
		"readlink":  "releases/" + fixtureReleaseID + "\n",
	}}
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if err != nil {
		t.Fatalf("RollbackEnvironment() error = %v", err)
	}
	if !strings.Contains(remote.callLog(), "ln -sfn -- 'releases/20260101T000000Z-abcdef123456'") {
		t.Fatalf("rollback must swap to the exact ledger previous target:\n%s", remote.callLog())
	}
}

// Acceptance: rollback after a deploy. The ledger line a real deploy appends
// is fed verbatim to a rollback, which must repoint current at the exact
// release the deploy recorded as previous= and health-check it — the
// cross-flow format contract, not a hand-written fixture.
func TestRollbackAfterDeployUsesRecordedPrevious(t *testing.T) {
	dir := writeDeployFixture(t)
	deployRemote := &fakeRemote{stdoutFor: map[string]string{
		"readlink": "releases/" + fixturePrevReleaseID + "\n",
	}}
	if _, _, err := runDeployEnv(t, baseEnvOpts(t, dir, deployRemote)); err != nil {
		t.Fatalf("DeployEnvironment() error = %v", err)
	}
	ledgerLine := appendedLedgerLine(t, deployRemote)

	rbRemote := &fakeRemote{stdoutFor: map[string]string{
		"tail -n 1": ledgerLine + "\n",
		"readlink":  "releases/" + fixtureReleaseID + "\n",
	}}
	if _, _, err := runRollback(t, baseRollbackOpts(t, dir, rbRemote)); err != nil {
		t.Fatalf("RollbackEnvironment() error = %v", err)
	}
	log := rbRemote.callLog()
	assertInOrder(t, log,
		"ln -sfn -- 'releases/"+fixturePrevReleaseID+"'",
		"sshin@h: curl",
		" rollback' >> '/opt/ouvrier/demo/deploys.log'",
	)
}

// appendedLedgerLine extracts the line a flow appended to deploys.log from
// the recorded printf command — the cross-flow ledger format contract.
func appendedLedgerLine(t *testing.T, remote *fakeRemote) string {
	t.Helper()
	var line string
	for _, cmd := range remote.sshCommands() {
		if !strings.Contains(cmd, ">> '/opt/ouvrier/demo/deploys.log'") {
			continue
		}
		_, rest, ok := strings.Cut(cmd, `printf '%s\n' '`)
		if !ok {
			t.Fatalf("unexpected ledger append shape: %q", cmd)
		}
		line, _, _ = strings.Cut(rest, `' >> `)
	}
	if line == "" {
		t.Fatalf("no ledger line appended:\n%s", remote.callLog())
	}
	return line
}

// Acceptance: chained rollback toggles like `cd -`. A first rollback moves
// current B→A and appends a rollback ledger entry recording previous=
// releases/<B>; running rollback again against that updated ledger repoints
// current back at B — fed verbatim from the first rollback's own append, not
// a hand-written fixture.
func TestRollbackTwiceTogglesBetweenReleases(t *testing.T) {
	dir := writeDeployFixture(t)

	// First rollback: the ledger says B replaced A, so current goes B→A.
	first := rollbackRemote()
	if _, _, err := runRollback(t, baseRollbackOpts(t, dir, first)); err != nil {
		t.Fatalf("first RollbackEnvironment() error = %v", err)
	}
	line := appendedLedgerLine(t, first)
	if want := fixturePrevReleaseID + " previous=releases/" + fixtureReleaseID + " rollback"; !strings.Contains(line, want) {
		t.Fatalf("first rollback ledger line = %q, want it to contain %q", line, want)
	}

	// Second rollback: the host now serves the rollback entry as its last
	// ledger line, and current points at A.
	second := &fakeRemote{stdoutFor: map[string]string{
		"tail -n 1": line + "\n",
		"readlink":  "releases/" + fixturePrevReleaseID + "\n",
	}}
	out, _, err := runRollback(t, baseRollbackOpts(t, dir, second))
	if err != nil {
		t.Fatalf("second RollbackEnvironment() error = %v", err)
	}
	assertInOrder(t, second.callLog(),
		"ln -sfn -- 'releases/"+fixtureReleaseID+"'",
		"sshin@h: curl",
		"printf '%s\\n' '2026-06-12T10:15:00Z "+fixtureReleaseID+" previous=releases/"+fixturePrevReleaseID+" rollback' >> '/opt/ouvrier/demo/deploys.log'",
	)
	if !strings.Contains(out.String(), "rolled back demo on h to "+fixtureReleaseID) {
		t.Fatalf("missing toggle-back success line:\n%s", out.String())
	}
}

// Acceptance: no previous release (first deploy, previous=-) refuses with an
// actionable message and leaves current untouched; the lock is still
// acquired and released.
func TestRollbackRefusesWhenNoPrevious(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{stdoutFor: map[string]string{
		"tail -n 1": "2026-06-12T10:15:00Z " + fixtureReleaseID + " previous=-\n",
	}}
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"release " + fixtureReleaseID, "no previous release", "first deploy", "current is untouched", "ouvrier deploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	assertCurrentUntouched(t, remote)
}

// Acceptance: a previous= value that is not a release this deploy created
// (hand-edited or corrupted ledger) refuses with corruption framing — fix or
// remove the line — distinct from the first-deploy previous=- refusal, and
// leaves current untouched.
func TestRollbackRefusesNonReleasePrevious(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{stdoutFor: map[string]string{
		"tail -n 1": "2026-06-12T10:15:00Z " + fixtureReleaseID + " previous=releases/garbage\n",
	}}
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{
		`previous="releases/garbage"`,
		"not a release this deploy created",
		"current is untouched",
		"fix or remove the line",
		"ouvrier deploy",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	assertCurrentUntouched(t, remote)
}

// Acceptance: a missing or empty deploys.log refuses with an actionable
// message and leaves current untouched.
func TestRollbackRefusesWithoutHistory(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{} // tail yields nothing: no ledger
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"no deploy history", "/opt/ouvrier/demo/deploys.log", "current is untouched", "ouvrier deploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	assertCurrentUntouched(t, remote)
}

// An unparseable last ledger line is a hard, explained refusal — not a guess.
func TestRollbackRefusesUnparseableLedgerLine(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := &fakeRemote{stdoutFor: map[string]string{"tail -n 1": "garbage line\n"}}
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), `"garbage line"`) {
		t.Fatalf("error = %v, want parse refusal quoting the line", err)
	}
	assertCurrentUntouched(t, remote)
}

// Acceptance: a pruned previous release directory refuses BEFORE the swap,
// leaving current untouched, with a message naming the directory and the way
// forward.
func TestRollbackRefusesPrunedPrevious(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := rollbackRemote()
	remote.failSSHContaining = "test -d"
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{
		"/opt/ouvrier/demo/releases/" + fixturePrevReleaseID,
		"pruned",
		"current is untouched",
		"ouvrier deploy",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	assertCurrentUntouched(t, remote)
}

// assertCurrentUntouched asserts a refused rollback never swapped or
// restarted anything, and still released the deploy lock it took.
func assertCurrentUntouched(t *testing.T, remote *fakeRemote) {
	t.Helper()
	log := remote.callLog()
	for _, banned := range []string{"ln -sfn", "mv -T", "systemctl restart", "curl", ">> "} {
		if strings.Contains(log, banned) {
			t.Fatalf("refusal must leave current untouched (found %q):\n%s", banned, log)
		}
	}
	assertInOrder(t, log,
		"flock -n '/opt/ouvrier/demo/.deploy.lock'",
		": > '/opt/ouvrier/demo/.deploy.lock'",
	)
}

// A failed health gate after the swap dumps journalctl, reports where current
// now points, appends NO ledger entry, and still releases the lock. No
// automatic counter-rollback: the operator picked this release deliberately.
func TestRollbackHealthFailureReportsAndReleasesLock(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := rollbackRemote()
	remote.failSSHInAll = true
	opts := baseRollbackOpts(t, dir, remote)
	_, _, err := runRollback(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, want := range []string{"health gate failed after 2 attempts", "current now points at " + fixturePrevReleaseID} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
	log := remote.callLog()
	assertInOrder(t, log,
		"ln -sfn -- 'releases/"+fixturePrevReleaseID+"'",
		"sudo /usr/bin/journalctl -u 'ouvrier-demo.service' -n 50 --no-pager",
		": > '/opt/ouvrier/demo/.deploy.lock'",
	)
	if strings.Contains(log, ">> '/opt/ouvrier/demo/deploys.log'") {
		t.Fatalf("a failed rollback must not append a ledger entry:\n%s", log)
	}
	inv, invErr := LoadInventory(opts.InventoryPath)
	if invErr != nil {
		t.Fatalf("LoadInventory: %v", invErr)
	}
	if len(inv.Deployments) != 0 {
		t.Fatalf("failed rollback must not touch the inventory: %+v", inv.Deployments)
	}
}

// Acceptance: a successful rollback records an honest inventory entry — the
// release ID that is now current, result "rollback-ok", and no sha/git rev
// (they are not recomputed).
func TestRollbackInventoryUpsert(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := rollbackRemote()
	opts := baseRollbackOpts(t, dir, remote)
	if _, _, err := runRollback(t, opts); err != nil {
		t.Fatalf("RollbackEnvironment() error = %v", err)
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
		Path:       "/opt/ouvrier/demo",
		Service:    "ouvrier-demo",
		AdminAddr:  "127.0.0.1:9090",
		HealthPath: "/admin/health",
		ReleaseID:  fixturePrevReleaseID,
		SHA256:     "",
		GitRev:     "",
		DeployedAt: fixedDeployTime,
		Result:     "rollback-ok",
	}
	if !d.DeployedAt.Equal(want.DeployedAt) {
		t.Fatalf("DeployedAt = %v, want %v", d.DeployedAt, want.DeployedAt)
	}
	d.DeployedAt = want.DeployedAt
	if d != want {
		t.Fatalf("inventory entry = %+v, want %+v", d, want)
	}
}

// Acceptance: same token discipline as deploy — the admin token is never in
// any argv or operator output; it travels only as the curl stdin config.
func TestRollbackTokenNeverInArgvOrOutput(t *testing.T) {
	for name, remote := range map[string]*fakeRemote{
		"happy": rollbackRemote(),
		"health failure": func() *fakeRemote {
			r := rollbackRemote()
			r.failSSHInAll = true
			r.echoCommandInError = true
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeDeployFixture(t)
			out, errOut, err := runRollback(t, baseRollbackOpts(t, dir, remote))
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

// The local env file must provide the admin token the health gate needs; the
// refusal happens before any remote command.
func TestRollbackRequiresAdminToken(t *testing.T) {
	dir := writeDeployFixture(t)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ANTHROPIC_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := rollbackRemote()
	_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "OUVRIER_ADMIN_TOKEN") {
		t.Fatalf("error = %v, want refusal naming OUVRIER_ADMIN_TOKEN", err)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("preflight failure must precede any remote command:\n%s", remote.callLog())
	}
}

// Rollback enforces the same host pinning gate as deploy, before anything.
func TestRollbackFailsFastWhenHostNotTrusted(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := rollbackRemote()
	opts := baseRollbackOpts(t, dir, remote)
	opts.Hosts = []string{"h", "untrusted.example.com"}
	_, _, err := runRollback(t, opts)
	if !errors.Is(err, ErrDeploy) || !strings.Contains(err.Error(), "ouvrier server trust untrusted.example.com") {
		t.Fatalf("error = %v, want trust hint", err)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("ran remote commands against untrusted host:\n%s", remote.callLog())
	}
}

// Acceptance: multi-host rollbacks are sequential, abort on the first
// failure, and shout about the resulting mixed-version fleet.
func TestRollbackMultiHostAbortsOnFirstFailure(t *testing.T) {
	dir := writeDeployFixture(t)
	remote := rollbackRemote()
	remote.failHost = "h2"
	remote.failSSHInAll = true
	opts := baseRollbackOpts(t, dir, remote)
	opts.Hosts = []string{"ops@h1", "ops@h2", "ops@h3"}
	opts.User = ""
	_, errOut, err := runRollback(t, opts)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("error = %v, want ErrDeploy", err)
	}
	for _, c := range remote.calls {
		if c.Host == "h3" {
			t.Fatalf("h3 must not be touched after the h2 failure:\n%s", remote.callLog())
		}
	}
	if !strings.Contains(remote.callLog(), "ssh@h1: : > ") {
		t.Fatalf("h1 rollback did not complete:\n%s", remote.callLog())
	}
	for _, want := range []string{
		"MIXED VERSIONS",
		"aborted after 1 of 3 hosts",
		"ok    ops@h1 (rolled back to " + fixturePrevReleaseID + ")",
		"FAIL  ops@h2",
		"skip  ops@h3 (not attempted)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("abort summary missing %q:\n%s", want, errOut.String())
		}
	}
	inv, invErr := LoadInventory(opts.InventoryPath)
	if invErr != nil {
		t.Fatalf("LoadInventory: %v", invErr)
	}
	if len(inv.Deployments) != 1 || inv.Deployments[0].Host != "h1" || inv.Deployments[0].Result != "rollback-ok" {
		t.Fatalf("inventory = %+v, want exactly the h1 rollback entry", inv.Deployments)
	}
}

// Every sudo invocation the rollback runs (probe, restart, journalctl) stays
// inside the deploy's least-privilege sudoers grant — no new grants needed.
func TestRollbackSudoCommandsCoveredBySudoers(t *testing.T) {
	dir := writeDeployFixture(t)
	for name, remote := range map[string]*fakeRemote{
		"happy": rollbackRemote(),
		"health failure": func() *fakeRemote {
			r := rollbackRemote()
			r.failSSHInAll = true
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runRollback(t, baseRollbackOpts(t, dir, remote))
			_ = err // failure paths must also stay within the grant

			snippet := SudoersSnippet(SudoersParams{
				DeployUser: "deploy", Name: "demo", Service: "ouvrier-demo", Root: "/opt/ouvrier/demo",
			})
			grants := map[string]bool{}
			for _, line := range strings.Split(snippet, "\n") {
				if after, ok := strings.CutPrefix(strings.TrimSpace(line), "deploy ALL=(root) NOPASSWD: "); ok {
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
						t.Fatalf("rollback sudo invocation %q has no sudoers grant; snippet:\n%s", argv, snippet)
					}
				}
			}
		})
	}
}

// The new release.go helpers: last-line read, existence probe, rollback
// ledger entry, and the parser they feed.
func TestRollbackLedgerHelpers(t *testing.T) {
	if got := ReadLastDeployLogCommand("/opt/x"); got != "tail -n 1 -- '/opt/x/deploys.log' 2>/dev/null || true" {
		t.Fatalf("ReadLastDeployLogCommand = %q", got)
	}
	if got := ReleaseDirExistsCommand("/opt/x", "20260612T101500Z-abc"); got != "test -d '/opt/x/releases/20260612T101500Z-abc'" {
		t.Fatalf("ReleaseDirExistsCommand = %q", got)
	}
	now := time.Date(2026, 6, 12, 10, 16, 0, 0, time.UTC)
	cmd := AppendRollbackLogCommand("/opt/x", "20260611T000000Z-old", "releases/20260612T101500Z-abc", now)
	want := "printf '%s\\n' '2026-06-12T10:16:00Z 20260611T000000Z-old previous=releases/20260612T101500Z-abc rollback' >> '/opt/x/deploys.log'"
	if cmd != want {
		t.Fatalf("AppendRollbackLogCommand = %q, want %q", cmd, want)
	}

	// Round trip: both deploy and rollback entries parse, marker ignored.
	for line, wantPrev := range map[string]string{
		fixtureLedgerLine: "releases/" + fixturePrevReleaseID,
		"2026-06-12T10:16:00Z 20260611T000000Z-old previous=releases/20260612T101500Z-abc rollback": "releases/20260612T101500Z-abc",
		"2026-06-12T10:15:00Z 20260612T101500Z-abc previous=-":                                      "-",
	} {
		_, prev, ok := parseDeployLogLine(line)
		if !ok || prev != wantPrev {
			t.Fatalf("parseDeployLogLine(%q) = %q, %v; want %q, true", line, prev, ok, wantPrev)
		}
	}
	for _, bad := range []string{"", "one two", "t id noprefix=x"} {
		if _, _, ok := parseDeployLogLine(bad); ok {
			t.Fatalf("parseDeployLogLine(%q) must fail", bad)
		}
	}
}
