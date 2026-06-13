package deploy

// release.go holds the building blocks for the release-layout deploy target
// (issue #44): release IDs and RELEASE.json metadata, plus pure helpers that
// render the remote command sequences (mkdir layout, symlink swap, deploy
// log, lock, prune) the orchestrated `ouvrier deploy <env>` flow (#45) feeds
// to a RemoteRunner. Everything here is deterministic and unit-testable
// without a real host; every remote path is shell-quoted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// DefaultKeepReleases is how many releases PruneReleasesCommands keeps when
// the caller passes keep <= 0 (`--keep` default).
const DefaultKeepReleases = 5

// envStageSuffix is where the deploy uploads the new dotenv before the
// privileged install moves it to <root>/shared/.env (root:svc 0640). The
// stage lives directly under <root> because shared/ is root-owned and the
// unprivileged deploy user could not write there.
const envStageSuffix = "/.env.new"

// releaseTimeLayout is the UTC timestamp prefix of a release ID. It is
// lexicographically sortable, so sorting release IDs as strings sorts them
// chronologically.
const releaseTimeLayout = "20060102T150405Z"

// releaseIDPattern matches IDs produced by ReleaseID. Prune helpers refuse
// to touch directory entries that do not match: the deploy never deletes
// anything it did not create.
var releaseIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-(?:[0-9a-f]{1,40}|nogit)$`)

// ReleaseID builds the immutable release directory name
// <UTCts>-<shortsha>. A missing or non-hex git SHA degrades to "nogit"
// (deploys from tarballs still work).
func ReleaseID(now time.Time, gitSHA string) string {
	short := strings.ToLower(strings.TrimSpace(gitSHA))
	if len(short) > 12 {
		short = short[:12]
	}
	if short == "" || !isHex(short) {
		short = "nogit"
	}
	return now.UTC().Format(releaseTimeLayout) + "-" + short
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ReleaseInfo is the RELEASE.json document written into every release
// directory; the console lists releases from it.
type ReleaseInfo struct {
	SHA256     string `json:"sha256"`            // hex sha256 of the shipped binary
	GitSHA     string `json:"git_sha,omitempty"` // full git HEAD sha, empty outside git
	GoVersion  string `json:"go_version"`        // toolchain that built the binary
	Builder    string `json:"builder"`           // user@hostname that ran the deploy
	DeployTime string `json:"deploy_time"`       // UTC RFC3339
}

// JSON renders the document with a trailing newline, ready to upload.
func (r ReleaseInfo) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// NewReleaseInfo collects the RELEASE.json fields for the binary at
// binaryPath built from the project at dir. Git metadata is best-effort
// (non-git checkouts yield an empty GitSHA); everything else is required.
func NewReleaseInfo(ctx context.Context, dir, binaryPath string, now time.Time) (ReleaseInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sha, err := fileSHA256(binaryPath)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("%w: hash release binary: %w", ErrDeploy, err)
	}
	return ReleaseInfo{
		SHA256:     sha,
		GitSHA:     gitHead(ctx, dir),
		GoVersion:  goToolchainVersion(ctx),
		Builder:    builderIdentity(),
		DeployTime: now.UTC().Format(time.RFC3339),
	}, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// gitHead returns `git rev-parse HEAD` for dir, or "" when dir is not a git
// checkout (or git is unavailable). Deploys must not require git.
func gitHead(ctx context.Context, dir string) string {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(out))
	if !isHex(strings.ToLower(sha)) || sha == "" {
		return ""
	}
	return sha
}

// goToolchainVersion reports the system `go version` (the toolchain that
// actually compiled the release), falling back to the version this CLI was
// built with when no toolchain is on PATH.
func goToolchainVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "go", "version").Output()
	if err == nil {
		// "go version go1.24.1 linux/amd64" -> "go1.24.1"
		fields := strings.Fields(string(out))
		if len(fields) >= 3 {
			return fields[2]
		}
	}
	return runtime.Version()
}

func builderIdentity() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	} else if env := os.Getenv("USER"); env != "" {
		name = env
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return name + "@" + host
}

// ReleaseDir is the immutable per-release directory under <root>/releases.
func ReleaseDir(root, releaseID string) string {
	return root + "/releases/" + releaseID
}

// MkdirLayoutCommands renders the remote preflight that materializes the
// fixed host layout under root:
//
//	<root>/                owned by the deploy user (uploads, symlink swap,
//	                       deploys.log, .deploy.lock all unprivileged)
//	<root>/releases/       immutable release dirs
//	<root>/shared/         root:ouvrier-<name> 0750 — the worker traverses,
//	                       the deploy user writes only via sudo install
//	<root>/shared/state/   ouvrier-<name>-owned (state.db lives here)
//
// owner is the remote deploy username (the account ssh logs in as); the
// sudoers snippet pins the same value, so resolve it remotely (`id -un`)
// before rendering when it is not known locally.
func MkdirLayoutCommands(root, name, owner string) []string {
	svc := UnitUser(name)
	return []string{
		fmt.Sprintf("sudo /usr/bin/install -d -m 0755 -o %s -- %s", shellQuote(owner), shellQuote(root)),
		fmt.Sprintf("mkdir -p -- %s", shellQuote(root+"/releases")),
		fmt.Sprintf("sudo /usr/bin/install -d -m 0750 -o root -g %s -- %s", shellQuote(svc), shellQuote(root+"/shared")),
		fmt.Sprintf("sudo /usr/bin/install -d -m 0750 -o %s -g %s -- %s", shellQuote(svc), shellQuote(svc), shellQuote(root+"/shared/state")),
	}
}

// CreateServiceUserCommand creates the dedicated nologin system user the
// unit runs as, idempotently: the unprivileged `id -u` guard means sudo only
// runs on the first deploy.
func CreateServiceUserCommand(root, name string) string {
	svc := UnitUser(name)
	return fmt.Sprintf(
		"id -u %s >/dev/null 2>&1 || sudo /usr/sbin/useradd --system --home-dir %s --no-create-home --shell /usr/sbin/nologin %s",
		shellQuote(svc), shellQuote(root), shellQuote(svc),
	)
}

// EnvStagePath is where the deploy uploads the new dotenv before
// InstallEnvCommands moves it into shared/.env with root:svc 0640.
func EnvStagePath(root string) string {
	return root + envStageSuffix
}

// InstallEnvCommands promotes the staged dotenv to <root>/shared/.env owned
// root:ouvrier-<name> mode 0640: the worker reads its secrets but can never
// rewrite them, and other users on the host cannot read them at all.
func InstallEnvCommands(root, name string) []string {
	svc := UnitUser(name)
	stage := EnvStagePath(root)
	return []string{
		fmt.Sprintf("sudo /usr/bin/install -o root -g %s -m 0640 -- %s %s",
			shellQuote(svc), shellQuote(stage), shellQuote(root+"/shared/.env")),
		fmt.Sprintf("rm -f -- %s", shellQuote(stage)),
	}
}

// ReadCurrentTargetCommand prints the relative target of the <root>/current
// symlink (e.g. "releases/20260612T101500Z-abc123def456"), or nothing on a
// first deploy. The caller records it in deploys.log so rollback can resolve
// the previous release without trusting timestamps.
func ReadCurrentTargetCommand(root string) string {
	return fmt.Sprintf("readlink -- %s || true", shellQuote(root+"/current"))
}

// SwapCurrentCommands atomically repoints <root>/current at the new release:
// build the symlink aside as current.tmp, then rename over the old one with
// GNU `mv -T` (one rename(2), no window where current is missing). The
// preflight probes `mv -T` support (`mv -T --help`, GNU-only); non-GNU
// userlands fall back to plain `ln -sfn`, which unlinks-then-relinks with a
// documented micro-race where current briefly does not resolve. The symlink
// target is relative so the layout survives a root move.
func SwapCurrentCommands(root, releaseID string) []string {
	target := shellQuote("releases/" + releaseID)
	tmp := shellQuote(root + "/current.tmp")
	current := shellQuote(root + "/current")
	return []string{
		fmt.Sprintf(
			"if mv -T --help >/dev/null 2>&1; then ln -sfn -- %s %s && mv -T -- %s %s; else ln -sfn -- %s %s; fi",
			target, tmp, tmp, current, target, current,
		),
	}
}

// AppendDeployLogCommand appends one line to the append-only
// <root>/deploys.log: deploy time, the release that just went live, and the
// `current` target it replaced ("-" on a first deploy). Rollback resolves
// the previous release from this log, never from timestamp sorting.
func AppendDeployLogCommand(root, releaseID, previousTarget string, now time.Time) string {
	prev := strings.TrimSpace(previousTarget)
	if prev == "" {
		prev = "-"
	}
	line := fmt.Sprintf("%s %s previous=%s", now.UTC().Format(time.RFC3339), releaseID, prev)
	return fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(line), shellQuote(root+"/deploys.log"))
}

// AcquireLockCommand takes the per-root deploy lock. The flock(1) on
// <root>/.deploy.lock makes the check-and-claim atomic against concurrent
// deploys; the holder description written into the lock file is what spans
// the deploy's separate ssh invocations and what a losing deploy prints as
// the held-by diagnostic. holder should identify the deployer (e.g.
// "user@host pid 123 release <id>"). ReleaseLockCommand must run on every
// exit path; a deploy killed mid-flight leaves a stale holder that the
// diagnostic makes easy to identify and clear (truncate the file).
func AcquireLockCommand(root, holder string) string {
	lock := shellQuote(root + "/.deploy.lock")
	inner := fmt.Sprintf(
		"if [ -s %s ]; then echo deploy lock %s held by: \"$(cat %s)\" >&2; exit 1; fi; printf '%%s\\n' %s > %s",
		lock, lock, lock, shellQuote(holder), lock,
	)
	return fmt.Sprintf("flock -n %s -c %s", lock, shellQuote(inner))
}

// ReleaseLockCommand releases the deploy lock by truncating the holder
// marker (the file itself stays, keeping the flock inode stable).
func ReleaseLockCommand(root string) string {
	return fmt.Sprintf(": > %s", shellQuote(root+"/.deploy.lock"))
}

// ListReleasesCommand lists the release directory entries one per line; its
// output feeds PruneReleasesCommands.
func ListReleasesCommand(root string) string {
	return fmt.Sprintf("ls -1 -- %s", shellQuote(root+"/releases"))
}

// PruneReleasesCommands builds the rm commands that keep only the newest
// keep releases (default DefaultKeepReleases when keep <= 0). The list is
// computed in Go from ListReleasesCommand output — never `head | xargs rm`
// on the remote — with every path individually quoted. Entries that do not
// match the release ID pattern are never touched, and release IDs sort
// lexicographically as chronologically thanks to the UTC timestamp prefix.
func PruneReleasesCommands(root, lsOutput string, keep int) []string {
	if keep <= 0 {
		keep = DefaultKeepReleases
	}
	var ids []string
	for _, line := range strings.Split(lsOutput, "\n") {
		id := strings.TrimSpace(line)
		if releaseIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) <= keep {
		return nil
	}
	stale := ids[:len(ids)-keep]
	cmds := make([]string, 0, len(stale))
	for _, id := range stale {
		cmds = append(cmds, "rm -rf -- "+shellQuote(ReleaseDir(root, id)))
	}
	return cmds
}
