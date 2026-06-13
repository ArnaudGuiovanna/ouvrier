package deploy

// deploy_env.go orchestrates `ouvrier deploy <env>` (and its `deploy ssh
// --host` alias): the agentless release-layout deploy specified in
// docs/superpowers/specs/2026-06-12-v0.3-deploy-and-scale-design.md §5.
//
// The eight steps, all through the RemoteRunner seam with pinned host keys:
//
//  1. Local preflight: env file resolution + validation, admin-addr policy,
//     static cross-compile (--target passthrough), sha256 + RELEASE.json.
//  2. Remote preflight: sudo -n probe (actionable error), then one ssh:
//     systemd check, service user, layout mkdir, .deploy.lock flock.
//  3. Upload the release (binary, RELEASE.json, skills/ assets) into the
//     immutable releases/<id>/ dir; verify the remote sha256; chmod 0755.
//  4. Ship the dotenv atomically: stage <root>/.env.new, then a privileged
//     install promotes it to shared/.env (root:svc 0640). The payload gets
//     OUVRIER_ADMIN_ADDR=127.0.0.1:9090 appended when the env file does not
//     set one; a non-loopback value is refused without AllowSharedAdmin.
//  5. Install the systemd unit only when its sha256 changed (+ daemon-reload),
//     enable it.
//  6. Record the previous `current` target, atomic symlink swap, restart.
//  7. Health gate: on-host curl against 127.0.0.1:<admin port>/admin/health,
//     10 attempts over ~30s. Token transport: a curl config (`header =
//     "Authorization: Bearer ..."`) fed to `curl -K -` over the ssh stdin
//     channel (RemoteRunner.SSHIn) — the token never appears in argv on
//     either machine and never touches a disk.
//  8. Success: append deploys.log, prune to --keep releases, upsert the
//     deployments inventory. Failure after the swap: dump journalctl, repoint
//     `current` to the recorded previous target, restart, re-run the health
//     gate, and return both errors; a first deploy (no previous) degrades to
//     systemctl stop + report.
//
// Multiple hosts deploy sequentially and abort on the first failure with a
// loud mixed-version summary. The remote .deploy.lock is released on every
// exit path once acquired.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// DefaultAdminAddr is the loopback admin listener address injected into the
// shipped .env when the operator's env file does not set OUVRIER_ADMIN_ADDR.
const DefaultAdminAddr = "127.0.0.1:9090"

const (
	defaultAdminPort      = "9090" // port component of DefaultAdminAddr
	defaultHealthAttempts = 10
	healthRetryDelay      = 3 * time.Second
)

// EnvOpts captures the resolved options for the release-layout deploy flow.
// The CLI fills it from `ouvrier deploy <env>` (pip.yaml deploy.<env> values
// as defaults) or from `ouvrier deploy ssh --host` (registry bypass).
type EnvOpts struct {
	Dir     string   // project directory; defaults to "."
	EnvName string   // deploy environment name; "" for the --host bypass
	Hosts   []string // deploy targets in order; entries may be user@host
	User    string   // ssh user override (--user); wins over user@host
	Port    int      // ssh port (0 = ssh default)
	Path    string   // remote install root; defaults to /opt/ouvrier/<name>
	Service string   // systemd unit name; defaults to ouvrier-<name>

	Identity string // ssh identity file (-i) for agent-less CI
	Target   string // GOOS/GOARCH cross-compile target; "" = linux/amd64
	Keep     int    // releases kept after prune; <=0 = DefaultKeepReleases

	// EnvFile is the explicit dotenv override (--env-file flag or
	// OUVRIER_DEPLOY_ENV_FILE, resolved by the caller). Empty falls back to
	// .env.<EnvName> then .env.
	EnvFile string
	// EnvRequired is pip.yaml's env.required list; the preflight reports
	// missing names (OUVRIER_ADMIN_TOKEN is always required).
	EnvRequired []string
	// AllowSharedAdmin permits an env file that points OUVRIER_ADMIN_ADDR at
	// a non-loopback interface. Off by default: a publicly shared admin
	// surface defeats "admin never exposed".
	AllowSharedAdmin bool

	UnitSandbox string // --unit-sandbox flag value ("", "on", "off")
	EnvSandbox  string // pip.yaml deploy.<env> sandbox value

	// GoRun is the `go build` seam; nil means DefaultGoRunner.
	GoRun GoRunner
	// Runner is the ssh/scp seam; nil means the system ssh/scp binaries.
	Runner RemoteRunner

	// Now and Sleep are clock seams for tests; nil means time.Now/time.Sleep.
	Now   func() time.Time
	Sleep func(time.Duration)
	// HealthAttempts overrides the health gate attempt count (tests); <=0
	// means the documented 10 attempts.
	HealthAttempts int
	// InventoryPath overrides the deployments inventory location; empty
	// resolves via InventoryPath() (OUVRIER_FLEET_PATH / OUVRIER_CONFIG_DIR).
	InventoryPath string
}

// DeployEnvironment runs the release-layout deploy against every host in
// opts.Hosts, sequentially, aborting on the first failure.
func DeployEnvironment(ctx context.Context, opts EnvOpts, progress ProgressWriter) error {
	if ctx == nil {
		ctx = context.Background()
	}
	progress = progress.normalized()
	p, err := planEnvDeploy(ctx, opts, progress)
	if err != nil {
		return err
	}
	// Defense in depth: whatever layer produced an error, the rendered
	// message never contains the admin token read from the env file.
	return maskTokenErr(p.run(ctx), p.token)
}

// envDeploy is the fully resolved deploy plan: everything local is done
// (build, hashes, payloads) before the first remote command runs.
type envDeploy struct {
	opts        EnvOpts
	out, errOut io.Writer
	runner      RemoteRunner

	name       string // project name from pip.yaml
	root       string // remote install root
	service    string // canonical unit name (no .service suffix)
	sandboxOff bool

	binPath     string // local built binary
	info        ReleaseInfo
	releaseJSON []byte
	releaseID   string
	releaseDir  string

	unit    string
	unitSHA string

	envPayload []byte // dotenv bytes shipped to the host
	token      string // OUVRIER_ADMIN_TOKEN from the env file (never logged)
	adminAddr  string // effective OUVRIER_ADMIN_ADDR on the host
	adminPort  string // health gate port

	now      func() time.Time
	sleep    func(time.Duration)
	attempts int
	keep     int
	invPath  string
}

func (p *envDeploy) mask(s string) string { return maskToken(s, p.token) }

// planEnvDeploy performs step 1 (local preflight + build) and resolves every
// value the per-host loop needs. It fails fast — before the build — when any
// target host is not pinned in ouvrier.known_hosts.
func planEnvDeploy(ctx context.Context, opts EnvOpts, progress ProgressWriter) (*envDeploy, error) {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if len(opts.Hosts) == 0 {
		return nil, fmt.Errorf("%w: deploy requires at least one host", ErrDeploy)
	}
	sandboxOff, err := ResolveSandbox(opts.UnitSandbox, opts.EnvSandbox)
	if err != nil {
		return nil, err
	}

	// Host-key pinning gate for every target, before the build and before any
	// remote command, so an untrusted host fails fast.
	for _, raw := range opts.Hosts {
		_, host := splitUserHost(raw)
		if _, _, err := requirePinnedHost(opts.Dir, host, opts.Port); err != nil {
			return nil, err
		}
	}

	// Env file: resolve, validate (git-tracked check, required names), and
	// load the values the flow needs (admin token, admin addr policy).
	envFile, err := ResolveEnvFile(opts.Dir, opts.EnvName, opts.EnvFile)
	if err != nil {
		return nil, err
	}
	if err := PreflightEnvFile(ctx, opts.Dir, envFile, opts.EnvRequired); err != nil {
		return nil, err
	}
	values, err := LoadDotenvFile(envFile)
	if err != nil {
		return nil, fmt.Errorf("%w: parse env file %s: %w", ErrDeploy, envFile, err)
	}
	token := strings.TrimSpace(values[envnames.AdminToken])
	adminAddr, adminPort, inject, err := planAdminAddr(values, opts.AllowSharedAdmin)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(envFile)
	if err != nil {
		return nil, fmt.Errorf("%w: read env file %s: %w", ErrDeploy, envFile, err)
	}
	if inject {
		// Append, never rewrite: the operator's file content ships verbatim.
		if len(payload) > 0 && payload[len(payload)-1] != '\n' {
			payload = append(payload, '\n')
		}
		payload = append(payload, []byte(envnames.AdminAddr+"="+DefaultAdminAddr+"\n")...)
	}
	fmt.Fprintf(progress.Out, "env file: %s (admin addr %s)\n", envFile, adminAddr)

	goRun := opts.GoRun
	if goRun == nil {
		goRun = DefaultGoRunner
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	br, err := StaticBuild(ctx, opts.Dir, opts.Target, progress.Out, progress.Err, goRun)
	if err != nil {
		return nil, err
	}
	buildTime := now()
	info, err := NewReleaseInfo(ctx, br.Dir, br.Output, buildTime)
	if err != nil {
		return nil, err
	}
	releaseJSON, err := info.JSON()
	if err != nil {
		return nil, fmt.Errorf("%w: encode RELEASE.json: %w", ErrDeploy, err)
	}
	releaseID := ReleaseID(buildTime, info.GitSHA)

	root := opts.Path
	if root == "" {
		root = "/opt/ouvrier/" + br.ProjectName
	}
	service := CanonicalServiceName(opts.Service)
	if service == "" {
		service = "ouvrier-" + br.ProjectName
	}
	unitParams := UnitParams{Name: br.ProjectName, Service: service, Root: root, SandboxOff: sandboxOff}
	if err := unitParams.Validate(); err != nil {
		return nil, err
	}
	unit := RenderUnitFile(unitParams)

	attempts := opts.HealthAttempts
	if attempts <= 0 {
		attempts = defaultHealthAttempts
	}
	keep := opts.Keep
	if keep <= 0 {
		keep = DefaultKeepReleases
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

	fmt.Fprintf(progress.Out, "release %s (sha256 %s)\n", releaseID, info.SHA256)
	return &envDeploy{
		opts:        opts,
		out:         progress.Out,
		errOut:      progress.Err,
		runner:      runner,
		name:        br.ProjectName,
		root:        root,
		service:     service,
		sandboxOff:  sandboxOff,
		binPath:     br.Output,
		info:        info,
		releaseJSON: releaseJSON,
		releaseID:   releaseID,
		releaseDir:  ReleaseDir(root, releaseID),
		unit:        unit,
		unitSHA:     UnitSHA256(unit),
		envPayload:  payload,
		token:       token,
		adminAddr:   adminAddr,
		adminPort:   adminPort,
		now:         now,
		sleep:       sleep,
		attempts:    attempts,
		keep:        keep,
		invPath:     invPath,
	}, nil
}

// planAdminAddr applies the admin exposure policy to the env file's
// OUVRIER_ADMIN_ADDR. An absent value is injected as the loopback default; a
// non-loopback value is refused unless allowShared. The returned port is the
// health gate target.
func planAdminAddr(values map[string]string, allowShared bool) (addr, port string, inject bool, err error) {
	raw := strings.TrimSpace(values[envnames.AdminAddr])
	if raw == "" {
		return DefaultAdminAddr, defaultAdminPort, true, nil
	}
	host, port, splitErr := net.SplitHostPort(raw)
	if splitErr != nil || strings.TrimSpace(port) == "" {
		return "", "", false, fmt.Errorf("%w: env file sets %s=%q; expected host:port (e.g. %s)", ErrDeploy, envnames.AdminAddr, raw, DefaultAdminAddr)
	}
	loopback := strings.EqualFold(host, "localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if !loopback && !allowShared {
		return "", "", false, fmt.Errorf(
			"%w: env file sets %s=%s, which exposes the admin API beyond loopback; bind it to 127.0.0.1, or pass --allow-shared-admin if this is deliberate",
			ErrDeploy, envnames.AdminAddr, raw,
		)
	}
	return raw, port, false, nil
}

// run executes the per-host deploy loop: sequential, abort on first failure,
// loud mixed-version summary when aborting after a partial rollout.
func (p *envDeploy) run(ctx context.Context) error {
	hosts := p.opts.Hosts
	for i, raw := range hosts {
		fmt.Fprintf(p.out, "==> deploying %s release %s to %s (%d/%d)\n", p.name, p.releaseID, raw, i+1, len(hosts))
		if err := p.deployHost(ctx, raw); err != nil {
			p.printAbortSummary(i, err)
			return err
		}
		fmt.Fprintf(p.out, "deployed %s release %s to %s (service=%s)\n", p.name, p.releaseID, raw, p.service)
	}
	return nil
}

// printAbortSummary reports per-host results when the loop stops at hosts[i],
// shouting when the fleet is now running mixed versions.
func (p *envDeploy) printAbortSummary(failed int, cause error) {
	hosts := p.opts.Hosts
	fmt.Fprintf(p.errOut, "deploy to %s failed: %s\n", hosts[failed], p.mask(cause.Error()))
	if len(hosts) == 1 {
		return
	}
	if failed > 0 {
		fmt.Fprintf(p.errOut, "WARNING: deploy aborted after %d of %d hosts — the fleet is running MIXED VERSIONS until the remaining hosts are deployed or the updated hosts are rolled back.\n", failed, len(hosts))
	} else {
		fmt.Fprintf(p.errOut, "deploy aborted on the first host; no other host was touched.\n")
	}
	for j, h := range hosts {
		switch {
		case j < failed:
			fmt.Fprintf(p.errOut, "  ok    %s (release %s)\n", h, p.releaseID)
		case j == failed:
			fmt.Fprintf(p.errOut, "  FAIL  %s\n", h)
		default:
			fmt.Fprintf(p.errOut, "  skip  %s (not attempted)\n", h)
		}
	}
}

// deployHost runs steps 2–8 against one host.
func (p *envDeploy) deployHost(ctx context.Context, rawHost string) error {
	hostUser, host := splitUserHost(rawHost)
	if p.opts.User != "" {
		hostUser = p.opts.User
	}
	knownHosts, pinnedHost, err := requirePinnedHost(p.opts.Dir, host, p.opts.Port)
	if err != nil {
		return err
	}
	runner := &pinnedRunner{inner: p.runner, host: pinnedHost}
	connect := ConnectOpts{
		Host:       host,
		User:       hostUser,
		Port:       p.opts.Port,
		Identity:   p.opts.Identity,
		KnownHosts: knownHosts,
	}

	// Step 2a: passwordless sudo probe with an actionable error before any
	// real work happens on the host.
	if _, err := runner.SSH(ctx, connect, SudoProbeCommand()); err != nil {
		return fmt.Errorf(
			"%w: %s: passwordless sudo is not configured for the deploy user (%v); install the least-privilege snippet from `ouvrier deploy ssh --print-sudoers` as /etc/sudoers.d/ouvrier-%s (mode 0440)",
			ErrDeploy, host, err, p.name,
		)
	}

	// The layout helpers and the sudoers snippet pin the deploy account that
	// owns <root>; resolve it remotely when not known locally.
	owner := hostUser
	if owner == "" {
		stdout, err := runner.SSH(ctx, connect, "id -un")
		if err != nil {
			return fmt.Errorf("%w: %s: resolve remote deploy user: %w", ErrDeploy, host, err)
		}
		owner = strings.TrimSpace(stdout)
		if owner == "" || len(strings.Fields(owner)) != 1 {
			return fmt.Errorf("%w: %s: could not determine the remote deploy user (`id -un` returned %q); pass --user or use user@host", ErrDeploy, host, owner)
		}
	}

	// Step 2b: one ssh — systemd check, service user, layout, deploy lock.
	// The lock is the final segment so a preflight failure never leaves a
	// stale holder behind.
	holder := fmt.Sprintf("%s pid %d release %s", builderIdentity(), os.Getpid(), p.releaseID)
	pre := []string{SystemdCheckCommand(), CreateServiceUserCommand(p.root, p.name)}
	pre = append(pre, MkdirLayoutCommands(p.root, p.name, owner)...)
	pre = append(pre, AcquireLockCommand(p.root, holder))
	if _, err := runner.SSH(ctx, connect, joinSegments(pre...)); err != nil {
		return fmt.Errorf("%w: %s: remote preflight failed: %w", ErrDeploy, host, err)
	}
	lockHeld := true
	defer func() {
		if !lockHeld {
			return
		}
		if _, rlErr := runner.SSH(ctx, connect, ReleaseLockCommand(p.root)); rlErr != nil {
			fmt.Fprintf(p.errOut, "WARN: %s: release deploy lock: %s\n", host, p.mask(rlErr.Error()))
		}
	}()

	// Step 3: upload the immutable release and verify it landed intact.
	for _, cmd := range MkdirReleaseCommands(p.root, p.releaseID) {
		if _, err := runner.SSH(ctx, connect, cmd); err != nil {
			return fmt.Errorf("%w: %s: create release directory: %w", ErrDeploy, host, err)
		}
	}
	remoteBin := p.releaseDir + "/bin/" + p.name
	fmt.Fprintf(p.out, "scp %s -> %s:%s\n", p.binPath, connect.userHost(), remoteBin)
	if err := runner.SCP(ctx, connect, p.binPath, remoteBin); err != nil {
		return fmt.Errorf("%w: %s: upload release binary: %w", ErrDeploy, host, err)
	}
	if err := runner.SCPData(ctx, connect, p.releaseJSON, p.releaseDir+"/RELEASE.json"); err != nil {
		return fmt.Errorf("%w: %s: upload RELEASE.json: %w", ErrDeploy, host, err)
	}
	if err := uploadRuntimeAssets(ctx, runner, connect, p.opts.Dir, p.releaseDir, p.out); err != nil {
		return err
	}
	if _, err := runner.SSH(ctx, connect, VerifyReleaseBinaryCommand(remoteBin, p.info.SHA256)); err != nil {
		return fmt.Errorf("%w: %s: remote sha256 verification failed for %s (expected %s): %w", ErrDeploy, host, remoteBin, p.info.SHA256, err)
	}

	// Step 4: ship the dotenv atomically — stage under <root>, then the
	// privileged install promotes it to shared/.env with root:svc 0640.
	stage := EnvStagePath(p.root)
	fmt.Fprintf(p.out, "ship env file -> %s:%s/shared/.env\n", connect.userHost(), p.root)
	if err := runner.SCPData(ctx, connect, p.envPayload, stage); err != nil {
		return fmt.Errorf("%w: %s: stage env file: %w", ErrDeploy, host, err)
	}
	if _, err := runner.SSH(ctx, connect, joinSegments(InstallEnvCommands(p.root, p.name)...)); err != nil {
		return fmt.Errorf("%w: %s: install env file: %w", ErrDeploy, host, err)
	}

	// Step 5: install the unit only when changed, then enable.
	if err := runner.SCPData(ctx, connect, []byte(p.unit), StagedUnitPath(p.root, p.service)); err != nil {
		return fmt.Errorf("%w: %s: upload systemd unit: %w", ErrDeploy, host, err)
	}
	if _, err := runner.SSH(ctx, connect, InstallUnitIfChangedCommand(p.root, p.service, p.unitSHA)); err != nil {
		return fmt.Errorf("%w: %s: install systemd unit: %w", ErrDeploy, host, err)
	}
	if _, err := runner.SSH(ctx, connect, EnableServiceCommand(p.service)); err != nil {
		return fmt.Errorf("%w: %s: enable service: %w", ErrDeploy, host, err)
	}

	// Step 6: record the previous release for rollback, swap, restart.
	prevOut, err := runner.SSH(ctx, connect, ReadCurrentTargetCommand(p.root))
	if err != nil {
		return fmt.Errorf("%w: %s: read current release: %w", ErrDeploy, host, err)
	}
	previous := strings.TrimSpace(prevOut)
	for _, cmd := range SwapCurrentCommands(p.root, p.releaseID) {
		if _, err := runner.SSH(ctx, connect, cmd); err != nil {
			return fmt.Errorf("%w: %s: switch current release: %w", ErrDeploy, host, err)
		}
	}
	if _, err := runner.SSH(ctx, connect, RestartServiceCommand(p.service)); err != nil {
		return p.failAfterSwap(ctx, runner, connect, host, previous,
			fmt.Errorf("%w: %s: systemctl restart failed: %w", ErrDeploy, host, err))
	}

	// Step 7: health gate.
	if err := p.healthGate(ctx, runner, connect); err != nil {
		return p.failAfterSwap(ctx, runner, connect, host, previous,
			fmt.Errorf("%w: %s: %w", ErrDeploy, host, err))
	}

	// Step 8: bookkeeping — deploy log, prune, lock release, inventory.
	if _, err := runner.SSH(ctx, connect, AppendDeployLogCommand(p.root, p.releaseID, previous, p.now())); err != nil {
		return fmt.Errorf("%w: %s: append deploys.log: %w", ErrDeploy, host, err)
	}
	lsOut, err := runner.SSH(ctx, connect, ListReleasesCommand(p.root))
	if err != nil {
		fmt.Fprintf(p.errOut, "WARN: %s: list releases for pruning: %s\n", host, p.mask(err.Error()))
	} else {
		for _, cmd := range PruneReleasesCommands(p.root, lsOut, p.keep) {
			if _, err := runner.SSH(ctx, connect, cmd); err != nil {
				fmt.Fprintf(p.errOut, "WARN: %s: prune old release: %s\n", host, p.mask(err.Error()))
			}
		}
	}
	lockHeld = false
	if _, err := runner.SSH(ctx, connect, ReleaseLockCommand(p.root)); err != nil {
		fmt.Fprintf(p.errOut, "WARN: %s: release deploy lock: %s\n", host, p.mask(err.Error()))
	}
	if err := p.upsertInventory(host, hostUser); err != nil {
		fmt.Fprintf(p.errOut, "WARN: record deployment in inventory: %s\n", p.mask(err.Error()))
	}
	return nil
}

// upsertInventory records the successful host deploy in the user-level
// deployments inventory. No secrets: admin_addr and health_path only.
func (p *envDeploy) upsertInventory(host, user string) error {
	return UpsertDeployment(p.invPath, Deployment{
		Name:       p.name,
		Host:       host,
		User:       user,
		Port:       p.opts.Port,
		Path:       p.root,
		Service:    p.service,
		AdminAddr:  p.adminAddr,
		HealthPath: "/admin/health",
		SHA256:     p.info.SHA256,
		GitRev:     p.info.GitSHA,
		DeployedAt: p.now().UTC(),
		Result:     "ok",
	})
}

// failAfterSwap is the post-swap failure path: capture journalctl, then roll
// back to the recorded previous release (swap + restart + health re-check),
// or degrade to stop+report on a first deploy. The returned error always
// carries the original cause plus the rollback outcome.
func (p *envDeploy) failAfterSwap(ctx context.Context, runner RemoteRunner, connect ConnectOpts, host, previous string, cause error) error {
	fmt.Fprintf(p.out, "deploy to %s failed: %s\n", host, p.mask(cause.Error()))
	if journal, jErr := runner.SSH(ctx, connect, JournalTailCommand(p.service)); jErr != nil {
		fmt.Fprintf(p.errOut, "WARN: %s: journalctl capture failed: %s\n", host, p.mask(jErr.Error()))
	} else {
		fmt.Fprintf(p.out, "---- journalctl -u %s.service -n 50 ----\n%s\n----\n", p.service, p.mask(strings.TrimSpace(journal)))
	}

	prevID, ok := previousReleaseID(p.root, previous)
	if !ok {
		// First deploy: nothing to roll back to. Stop the unit so systemd
		// does not crash-loop a broken release, and report.
		fmt.Fprintf(p.out, "no previous release on %s; stopping %s\n", host, p.service)
		if _, stopErr := runner.SSH(ctx, connect, StopServiceCommand(p.service)); stopErr != nil {
			return fmt.Errorf("%w (no previous release to roll back to; systemctl stop also failed: %v)", cause, stopErr)
		}
		return fmt.Errorf("%w (no previous release to roll back to; service stopped)", cause)
	}

	fmt.Fprintf(p.out, "rolling back %s to %s\n", host, prevID)
	rbErr := func() error {
		for _, cmd := range SwapCurrentCommands(p.root, prevID) {
			if _, err := runner.SSH(ctx, connect, cmd); err != nil {
				return fmt.Errorf("repoint current: %w", err)
			}
		}
		if _, err := runner.SSH(ctx, connect, RestartServiceCommand(p.service)); err != nil {
			return fmt.Errorf("restart previous release: %w", err)
		}
		return p.healthGate(ctx, runner, connect)
	}()
	if rbErr != nil {
		return fmt.Errorf("%w (rollback to %s also failed: %v)", cause, prevID, rbErr)
	}
	fmt.Fprintf(p.out, "rolled back %s to %s (health OK)\n", host, prevID)
	return fmt.Errorf("%w (rolled back to %s, health OK)", cause, prevID)
}

// healthGate probes the worker's loopback admin health endpoint on the host:
// up to p.attempts curl attempts, sleeping between them (10 attempts over
// ~30s by default). The bearer token travels as a curl config on the ssh
// stdin channel (`curl -K -`), never in argv and never on disk.
func (p *envDeploy) healthGate(ctx context.Context, runner RemoteRunner, connect ConnectOpts) error {
	cmd := healthGateCommand(p.adminPort)
	cfg := curlAuthConfig(p.token)
	var last error
	for attempt := 1; attempt <= p.attempts; attempt++ {
		if attempt > 1 {
			p.sleep(healthRetryDelay)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("health gate interrupted: %w", err)
		}
		if _, err := runner.SSHIn(ctx, connect, cmd, cfg); err == nil {
			fmt.Fprintf(p.out, "health OK: http://127.0.0.1:%s/admin/health (attempt %d/%d)\n", p.adminPort, attempt, p.attempts)
			return nil
		} else {
			last = err
		}
	}
	return fmt.Errorf("health gate failed after %d attempts against 127.0.0.1:%s/admin/health: %w", p.attempts, p.adminPort, last)
}

// healthGateCommand renders the on-host probe. -K - makes curl read its
// config (the Authorization header) from stdin; -f maps non-2xx to a failure.
func healthGateCommand(port string) string {
	return "curl -fsS -o /dev/null --max-time 5 -K - " + shellQuote("http://127.0.0.1:"+port+"/admin/health")
}

// curlAuthConfig renders the curl config carrying the bearer token, with
// curl-config string escaping for backslashes and double quotes.
func curlAuthConfig(token string) []byte {
	v := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace("Authorization: Bearer " + token)
	return []byte(`header = "` + v + "\"\n")
}

// previousReleaseID extracts the release ID from a `current` symlink target
// recorded before the swap. Only targets the deploy itself created (matching
// the release ID pattern) are eligible rollback targets.
func previousReleaseID(root, target string) (string, bool) {
	t := strings.TrimSpace(target)
	t = strings.TrimPrefix(t, root+"/")
	id, ok := strings.CutPrefix(t, "releases/")
	if !ok || !releaseIDPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// splitUserHost splits an optional user@ prefix off a deploy target.
func splitUserHost(s string) (user, host string) {
	s = strings.TrimSpace(s)
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[:at], s[at+1:]
	}
	return "", s
}

// joinSegments chains remote commands with && so a failure stops the chain.
// Segments containing their own control operators are brace-grouped so the
// chain's && cannot rebind a segment's internal || (e.g. the idempotent
// useradd guard in CreateServiceUserCommand).
func joinSegments(cmds ...string) string {
	parts := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if strings.ContainsAny(c, "|&;") {
			c = "{ " + c + "; }"
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " && ")
}

// uploadRuntimeAssets ships runtime assets that are intentionally read from
// disk by the worker — skills/<name>/SKILL.md and friends — into the release
// directory, mirroring the local skills/ tree.
func uploadRuntimeAssets(ctx context.Context, ssh RemoteRunner, connect ConnectOpts, localRoot, remoteRoot string, out io.Writer) error {
	skillsRoot := filepath.Join(localRoot, "skills")
	info, err := os.Stat(skillsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: stat runtime assets: %w", ErrDeploy, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s exists but is not a directory", ErrDeploy, skillsRoot)
	}

	return filepath.WalkDir(skillsRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: read runtime asset %s: %w", ErrDeploy, path, walkErr)
		}
		rel, err := filepath.Rel(localRoot, path)
		if err != nil {
			return fmt.Errorf("%w: resolve runtime asset %s: %w", ErrDeploy, path, err)
		}
		remoteAssetPath := remoteRoot + "/" + filepath.ToSlash(rel)
		if entry.IsDir() {
			mkdirCmd := fmt.Sprintf("mkdir -p -- %s", shellQuote(remoteAssetPath))
			if _, err := ssh.SSH(ctx, connect, mkdirCmd); err != nil {
				return fmt.Errorf("%w: create remote runtime asset directory: %w", ErrDeploy, err)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%w: stat runtime asset %s: %w", ErrDeploy, path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fmt.Fprintf(out, "scp %s -> %s:%s\n", rel, connect.userHost(), remoteAssetPath)
		if err := ssh.SCP(ctx, connect, path, remoteAssetPath); err != nil {
			return fmt.Errorf("%w: upload runtime asset %s: %w", ErrDeploy, rel, err)
		}
		return nil
	})
}
