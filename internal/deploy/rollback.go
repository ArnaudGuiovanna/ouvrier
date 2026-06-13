package deploy

// rollback.go orchestrates `ouvrier deploy rollback <env>` (issue #46): the
// operator-initiated counterpart of the auto-rollback inside a failed deploy
// (deploy_env.go failAfterSwap). There is no build and no upload — per host,
// the flow resolves the previous release from the last <root>/deploys.log
// entry (the ledger records the actual `current` target each deploy
// replaced, so rollback never trusts timestamp sorting), verifies that
// release directory still exists, atomically repoints `current`, restarts
// the unit, and runs the same health gate as a deploy. Success appends a
// distinguishable "rollback" ledger entry and updates the deployments
// inventory (result "rollback-ok").
//
// The host's shared/.env is deliberately NOT rolled back: the latest shipped
// secrets stay in place (per-release env snapshotting is a pending design
// decision). The local env file is read only for the admin token and address
// the health gate needs — it must match what is deployed.
//
// Rollback mutates `current`, so it takes the same <root>/.deploy.lock as a
// deploy, with a "rollback" holder marker, released on every exit path.
//
// Deliberate file-size exception (AGENTS.md), mirroring deploy_env.go: the
// plan + per-host loop form one sequential protocol whose steps are only
// meaningful in order; splitting them would hide the sequence the tests pin.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// RollbackOpts captures the resolved options for the rollback flow: the
// deploy EnvOpts minus everything build- or upload-related (target, keep,
// sandbox, env payload).
type RollbackOpts struct {
	Dir     string   // project directory; defaults to "."
	EnvName string   // deploy environment name; "" for the --host bypass
	Hosts   []string // rollback targets in order; entries may be user@host
	User    string   // ssh user override (--user); wins over user@host
	Port    int      // ssh port (0 = ssh default)
	Path    string   // remote install root; defaults to /opt/ouvrier/<name>
	Service string   // systemd unit name; defaults to ouvrier-<name>

	Identity string // ssh identity file (-i) for agent-less CI

	// EnvFile is the explicit dotenv override; empty falls back to
	// .env.<EnvName> then .env. Rollback reads it ONLY for the admin token and
	// admin addr the health gate needs — nothing is shipped, and the token
	// must match the one already deployed to the host.
	EnvFile string
	// AllowSharedAdmin permits an env file that points OUVRIER_ADMIN_ADDR at
	// a non-loopback interface (same policy as deploy).
	AllowSharedAdmin bool

	// Runner is the ssh seam; nil means the system ssh binary.
	Runner RemoteRunner
	// Now and Sleep are clock seams for tests; nil means time.Now/time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
	// HealthAttempts overrides the health gate attempt count (tests); <=0
	// means the documented 10 attempts.
	HealthAttempts int
	// InventoryPath overrides the deployments inventory location; empty
	// resolves via InventoryPath().
	InventoryPath string
}

// RollbackEnvironment rolls every host in opts.Hosts back to the previous
// release recorded in its deploys.log, sequentially, aborting on the first
// failure.
func RollbackEnvironment(ctx context.Context, opts RollbackOpts, progress ProgressWriter) error {
	if ctx == nil {
		ctx = context.Background()
	}
	progress = progress.normalized()
	p, err := planRollback(opts, progress)
	if err != nil {
		return err
	}
	return MaskTokenErr(p.run(ctx), p.token)
}

// envRollback is the fully resolved rollback plan.
type envRollback struct {
	opts        RollbackOpts
	out, errOut io.Writer
	runner      RemoteRunner

	name    string // project name from pip.yaml
	root    string // remote install root
	service string // canonical unit name (no .service suffix)

	token     string // OUVRIER_ADMIN_TOKEN from the local env file (never logged)
	adminAddr string // effective OUVRIER_ADMIN_ADDR recorded in the inventory
	adminPort string // health gate port

	now      func() time.Time
	sleep    func(time.Duration)
	attempts int
	invPath  string
}

func (p *envRollback) mask(s string) string { return MaskToken(s, p.token) }

// planRollback performs the local preflight: pinned-host gate for every
// target, project name from pip.yaml, root/service defaults and safety
// validation, and the env-file read the health gate needs. No build.
func planRollback(opts RollbackOpts, progress ProgressWriter) (*envRollback, error) {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if len(opts.Hosts) == 0 {
		return nil, fmt.Errorf("%w: rollback requires at least one host", ErrDeploy)
	}
	for _, raw := range opts.Hosts {
		_, host := splitUserHost(raw)
		if _, _, err := RequirePinnedHost(opts.Dir, host, opts.Port); err != nil {
			return nil, err
		}
	}

	pipPath := filepath.Join(opts.Dir, "pip.yaml")
	data, err := os.ReadFile(pipPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v (run rollback from the Ouvrier project that was deployed)", ErrDeploy, pipPath, err)
	}
	name, err := ParseProjectName(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDeploy, err)
	}

	root := opts.Path
	if root == "" {
		root = "/opt/ouvrier/" + name
	}
	service := CanonicalServiceName(opts.Service)
	if service == "" {
		service = "ouvrier-" + name
	}
	if err := (UnitParams{Name: name, Service: service, Root: root}).Validate(); err != nil {
		return nil, err
	}

	// The local env file supplies only what the health gate needs: the admin
	// token (sent over the ssh stdin channel) and the admin port. The host's
	// shared/.env is NOT touched, so this file's token must match it.
	envFile, err := ResolveEnvFile(opts.Dir, opts.EnvName, opts.EnvFile)
	if err != nil {
		return nil, err
	}
	values, err := LoadDotenvFile(envFile)
	if err != nil {
		return nil, fmt.Errorf("%w: parse env file %s: %w", ErrDeploy, envFile, err)
	}
	token := strings.TrimSpace(values[envnames.AdminToken])
	if token == "" {
		return nil, fmt.Errorf(
			"%w: env file %s does not set %s; the rollback health gate needs the admin token already deployed to the host",
			ErrDeploy, envFile, envnames.AdminToken,
		)
	}
	adminAddr, adminPort, _, err := planAdminAddr(values, opts.AllowSharedAdmin)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(progress.Out, "env file: %s (admin addr %s; health gate only — the host's .env is not rolled back)\n", envFile, adminAddr)

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	attempts := opts.HealthAttempts
	if attempts <= 0 {
		attempts = defaultHealthAttempts
	}
	invPath := opts.InventoryPath
	if invPath == "" {
		if invPath, err = InventoryPath(); err != nil {
			return nil, err
		}
	}
	runner := opts.Runner
	if runner == nil {
		runner = defaultRemoteRunner{}
	}

	return &envRollback{
		opts:      opts,
		out:       progress.Out,
		errOut:    progress.Err,
		runner:    runner,
		name:      name,
		root:      root,
		service:   service,
		token:     token,
		adminAddr: adminAddr,
		adminPort: adminPort,
		now:       now,
		sleep:     sleep,
		attempts:  attempts,
		invPath:   invPath,
	}, nil
}

// run executes the per-host rollback loop: sequential, abort on first
// failure, loud mixed-version summary when aborting after a partial fleet.
func (p *envRollback) run(ctx context.Context) error {
	hosts := p.opts.Hosts
	done := make([]string, 0, len(hosts)) // release each host was rolled back to
	for i, raw := range hosts {
		fmt.Fprintf(p.out, "==> rolling back %s on %s (%d/%d)\n", p.name, raw, i+1, len(hosts))
		releaseID, err := p.rollbackHost(ctx, raw)
		if err != nil {
			p.printAbortSummary(done, err)
			return err
		}
		fmt.Fprintf(p.out, "rolled back %s on %s to %s (service=%s)\n", p.name, raw, releaseID, p.service)
		done = append(done, releaseID)
	}
	return nil
}

// printAbortSummary reports per-host results when the loop stops at
// hosts[len(done)], shouting when the fleet now runs mixed versions.
func (p *envRollback) printAbortSummary(done []string, cause error) {
	hosts := p.opts.Hosts
	failed := len(done)
	fmt.Fprintf(p.errOut, "rollback on %s failed: %s\n", hosts[failed], p.mask(cause.Error()))
	if len(hosts) == 1 {
		return
	}
	if failed > 0 {
		fmt.Fprintf(p.errOut, "WARNING: rollback aborted after %d of %d hosts — the fleet is running MIXED VERSIONS until the remaining hosts are rolled back or redeployed.\n", failed, len(hosts))
	} else {
		fmt.Fprintf(p.errOut, "rollback aborted on the first host; no other host was touched.\n")
	}
	for j, h := range hosts {
		switch {
		case j < failed:
			fmt.Fprintf(p.errOut, "  ok    %s (rolled back to %s)\n", h, done[j])
		case j == failed:
			fmt.Fprintf(p.errOut, "  FAIL  %s\n", h)
		default:
			fmt.Fprintf(p.errOut, "  skip  %s (not attempted)\n", h)
		}
	}
}

// rollbackHost rolls one host back to the previous release recorded in its
// deploys.log and returns that release ID. Every refusal before the symlink
// swap leaves `current` untouched.
func (p *envRollback) rollbackHost(ctx context.Context, rawHost string) (string, error) {
	hostUser, host := splitUserHost(rawHost)
	if p.opts.User != "" {
		hostUser = p.opts.User
	}
	knownHosts, pinnedHost, err := RequirePinnedHost(p.opts.Dir, host, p.opts.Port)
	if err != nil {
		return "", err
	}
	runner := &pinnedRunner{inner: p.runner, host: pinnedHost}
	connect := ConnectOpts{
		Host:       host,
		User:       hostUser,
		Port:       p.opts.Port,
		Identity:   p.opts.Identity,
		KnownHosts: knownHosts,
	}

	// Passwordless sudo probe first: restart needs it, with an actionable
	// error before anything happens on the host.
	if _, err := runner.SSH(ctx, connect, SudoProbeCommand()); err != nil {
		return "", fmt.Errorf(
			"%w: %s: passwordless sudo is not configured for the deploy user (%v); install the least-privilege snippet from `ouvrier deploy ssh --print-sudoers` as /etc/sudoers.d/ouvrier-%s (mode 0440)",
			ErrDeploy, host, err, p.name,
		)
	}

	// Rollback mutates `current`: take the same deploy lock, released on
	// every exit path once acquired.
	holder := fmt.Sprintf("%s pid %d rollback", builderIdentity(), os.Getpid())
	if _, err := runner.SSH(ctx, connect, AcquireLockCommand(p.root, holder)); err != nil {
		return "", fmt.Errorf("%w: %s: acquire deploy lock: %w", ErrDeploy, host, err)
	}
	lockHeld := true
	releaseLock := func() {
		if !lockHeld {
			return
		}
		lockHeld = false
		if _, rlErr := runner.SSH(ctx, connect, ReleaseLockCommand(p.root)); rlErr != nil {
			fmt.Fprintf(p.errOut, "WARN: %s: release deploy lock: %s\n", host, p.mask(rlErr.Error()))
		}
	}
	defer releaseLock()

	// Resolve the rollback target from the LAST ledger entry — the recorded
	// previous `current` target — never from timestamp sorting.
	logPath := p.root + "/deploys.log"
	lastOut, err := runner.SSH(ctx, connect, ReadLastDeployLogCommand(p.root))
	if err != nil {
		return "", fmt.Errorf("%w: %s: read %s: %w", ErrDeploy, host, logPath, err)
	}
	lastLine := strings.TrimSpace(lastOut)
	if lastLine == "" {
		return "", fmt.Errorf(
			"%w: %s: no deploy history in %s; nothing to roll back and current is untouched — deploy a release first with `ouvrier deploy`",
			ErrDeploy, host, logPath,
		)
	}
	deployedID, previousTarget, ok := parseDeployLogLine(lastLine)
	if !ok {
		return "", fmt.Errorf(
			"%w: %s: cannot parse the last %s entry %q; current is untouched — fix or remove the line, or redeploy a known-good revision with `ouvrier deploy`",
			ErrDeploy, host, logPath, lastLine,
		)
	}
	prevID, ok := previousReleaseID(p.root, previousTarget)
	if !ok {
		if previousTarget == "-" {
			return "", fmt.Errorf(
				"%w: %s: the last deploy (release %s) recorded no previous release — a first deploy has nothing to roll back to; current is untouched. Deploy a known-good revision instead with `ouvrier deploy`",
				ErrDeploy, host, deployedID,
			)
		}
		return "", fmt.Errorf(
			"%w: %s: the last %s entry records previous=%q, which is not a release this deploy created; current is untouched — fix or remove the line, or redeploy a known-good revision with `ouvrier deploy`",
			ErrDeploy, host, logPath, previousTarget,
		)
	}

	// Verify the target release still exists BEFORE touching current.
	if _, err := runner.SSH(ctx, connect, ReleaseDirExistsCommand(p.root, prevID)); err != nil {
		return "", fmt.Errorf(
			"%w: %s: previous release directory %s no longer exists (pruned by --keep?); current is untouched — redeploy the wanted revision with `ouvrier deploy` instead (%v)",
			ErrDeploy, host, ReleaseDir(p.root, prevID), err,
		)
	}

	// Record what is current right now for the new ledger entry's previous=.
	curOut, err := runner.SSH(ctx, connect, ReadCurrentTargetCommand(p.root))
	if err != nil {
		return "", fmt.Errorf("%w: %s: read current release: %w", ErrDeploy, host, err)
	}
	currentTarget := strings.TrimSpace(curOut)

	fmt.Fprintf(p.out, "rolling back %s to %s (current was %s)\n", host, prevID, currentTarget)
	for _, cmd := range SwapCurrentCommands(p.root, prevID) {
		if _, err := runner.SSH(ctx, connect, cmd); err != nil {
			return "", fmt.Errorf("%w: %s: switch current release: %w", ErrDeploy, host, err)
		}
	}
	if _, err := runner.SSH(ctx, connect, RestartServiceCommand(p.service)); err != nil {
		return "", p.reportFailedRollback(ctx, runner, connect, host, prevID,
			fmt.Errorf("%w: %s: systemctl restart failed: %w", ErrDeploy, host, err))
	}
	if err := runHealthGate(ctx, runner, connect, p.adminPort, p.token, p.attempts, p.sleep, p.out); err != nil {
		return "", p.reportFailedRollback(ctx, runner, connect, host, prevID,
			fmt.Errorf("%w: %s: %w", ErrDeploy, host, err))
	}

	// Bookkeeping: rollback ledger entry, lock release, inventory.
	if _, err := runner.SSH(ctx, connect, AppendRollbackLogCommand(p.root, prevID, currentTarget, p.now())); err != nil {
		return "", fmt.Errorf("%w: %s: append %s: %w", ErrDeploy, host, logPath, err)
	}
	releaseLock()
	if err := p.upsertInventory(host, hostUser, prevID); err != nil {
		fmt.Fprintf(p.errOut, "WARN: record rollback in inventory: %s\n", p.mask(err.Error()))
	}
	return prevID, nil
}

// reportFailedRollback is the post-swap failure path: capture journalctl and
// return the cause annotated with where `current` now points. Unlike a
// deploy, there is no automatic counter-rollback — the operator chose this
// release deliberately and the way forward is redeploying a good revision.
func (p *envRollback) reportFailedRollback(ctx context.Context, runner RemoteRunner, connect ConnectOpts, host, prevID string, cause error) error {
	fmt.Fprintf(p.errOut, "rollback on %s failed: %s\n", host, p.mask(cause.Error()))
	if journal, jErr := runner.SSH(ctx, connect, JournalTailCommand(p.service)); jErr != nil {
		fmt.Fprintf(p.errOut, "WARN: %s: journalctl capture failed: %s\n", host, p.mask(jErr.Error()))
	} else {
		fmt.Fprintf(p.out, "---- journalctl -u %s.service -n 50 ----\n%s\n----\n", p.service, p.mask(strings.TrimSpace(journal)))
	}
	return fmt.Errorf("%w (current now points at %s; deploy a known-good revision with `ouvrier deploy`)", cause, prevID)
}

// upsertInventory records the successful host rollback. The binary's sha256
// and git revision are not recomputed for a rollback — the entry honestly
// records only the release ID that is now current.
func (p *envRollback) upsertInventory(host, user, releaseID string) error {
	return UpsertDeployment(p.invPath, Deployment{
		Name:       p.name,
		Host:       host,
		User:       user,
		Port:       p.opts.Port,
		Path:       p.root,
		Service:    p.service,
		AdminAddr:  p.adminAddr,
		HealthPath: "/admin/health",
		ReleaseID:  releaseID,
		DeployedAt: p.now().UTC(),
		Result:     "rollback-ok",
	})
}
