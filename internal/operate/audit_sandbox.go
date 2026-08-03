package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

const (
	auditSandboxWorkspace = "/workspace"
	auditSandboxCache     = "/cache"
	auditSandboxTemp      = "/tmp"
	auditSandboxOutput    = "/output"
)

// auditSandbox owns one disposable source stage. Dependency resolution is
// performed before any candidate test binary runs. All executable gates then
// see only the staged worker (read-only), its vendor directory, an empty
// environment, and private cache/temp/output mounts.
type auditSandbox struct {
	sourceDir string
	rootDir   string
	stageDir  string
	cacheDir  string
	tempDir   string
	outputDir string
	bwrap     string
	goBinary  string
	goRoot    string
}

type auditModuleEdit struct {
	Replace []struct {
		Old auditModuleVersion `json:"Old"`
		New auditModuleVersion `json:"New"`
	} `json:"Replace"`
}

type auditModuleVersion struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
}

type auditSandboxMount struct {
	source      string
	destination string
	writable    bool
}

func newAuditSandbox(ctx context.Context, dir string, expected SourceSnapshot) (_ *auditSandbox, retErr error) {
	ctx, cancel := auditTimeoutContext(ctx)
	defer cancel()
	if err := auditSandboxPlatformCheck(); err != nil {
		return nil, fmt.Errorf("operate: audit sandbox unavailable: %w", err)
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("operate: audit sandbox unavailable: bubblewrap not found: %w", err)
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("operate: audit sandbox unavailable: Go toolchain not found: %w", err)
	}
	goBinary, err = filepath.EvalSymlinks(goBinary)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve audit Go toolchain: %w", err)
	}
	goRoot := filepath.Dir(filepath.Dir(goBinary))
	if _, err := os.Stat(filepath.Join(goRoot, "pkg", "tool")); err != nil {
		return nil, fmt.Errorf("operate: derived audit GOROOT %s is invalid: %w", goRoot, err)
	}
	_, sourceReal, err := realDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve audit sandbox source: %w", err)
	}
	if expected.Workspace != sourceReal {
		return nil, fmt.Errorf("operate: audit sandbox source no longer matches the fingerprinted workspace")
	}

	stateDir, err := ensureWorkerStateDir(sourceReal, "audit-sandboxes")
	if err != nil {
		return nil, err
	}
	rootDir, err := os.MkdirTemp(stateDir, "run-")
	if err != nil {
		return nil, fmt.Errorf("operate: create audit sandbox state: %w", err)
	}
	sandbox := &auditSandbox{
		sourceDir: sourceReal,
		rootDir:   rootDir,
		stageDir:  filepath.Join(rootDir, "workspace"),
		cacheDir:  filepath.Join(rootDir, "cache"),
		tempDir:   filepath.Join(rootDir, "tmp"),
		outputDir: filepath.Join(rootDir, "output"),
		bwrap:     bwrap,
		goBinary:  goBinary,
		goRoot:    goRoot,
	}
	defer func() {
		if retErr != nil {
			_ = sandbox.Close()
		}
	}()
	for _, path := range []string{sandbox.stageDir, sandbox.cacheDir, sandbox.tempDir, sandbox.outputDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("operate: create audit sandbox directory: %w", err)
		}
	}
	sourceBeforeCopy, err := sourceTreeSnapshot(sourceReal)
	if err != nil {
		return nil, fmt.Errorf("operate: fingerprint worker tree before sandbox staging: %w", err)
	}
	if err := copyAuditSource(ctx, sourceReal, sandbox.stageDir); err != nil {
		return nil, err
	}
	liveSanitized, err := stableDriverStageTree(ctx, sourceReal)
	if err != nil {
		return nil, fmt.Errorf("operate: verify sanitized audit source: %w", err)
	}
	stagedSanitized, err := stableDriverStageTree(ctx, sandbox.stageDir)
	if err != nil {
		return nil, fmt.Errorf("operate: verify staged audit source: %w", err)
	}
	if !equalDriverStageTrees(liveSanitized, stagedSanitized) {
		return nil, fmt.Errorf("operate: audit sandbox stage does not match the sanitized worker source")
	}
	afterCopy, err := sourceTreeSnapshot(sourceReal)
	if err != nil {
		return nil, fmt.Errorf("operate: fingerprint worker tree after sandbox staging: %w", err)
	}
	if sourceBeforeCopy != afterCopy || afterCopy.Workspace != expected.Workspace {
		return nil, fmt.Errorf("operate: worker source changed while the audit sandbox was being staged")
	}
	if err := sandbox.prepareVendor(ctx); err != nil {
		return nil, err
	}
	if err := sandbox.preflight(ctx); err != nil {
		return nil, err
	}
	return sandbox, nil
}

// Close removes all writable state used by one audit. The directory is under
// the already validated .ouvrier state root and never contains user source.
func (s *auditSandbox) Close() error {
	if s == nil || s.rootDir == "" {
		return nil
	}
	if err := os.RemoveAll(s.rootDir); err != nil {
		return fmt.Errorf("operate: remove audit sandbox state: %w", err)
	}
	return nil
}

// RunGo executes only the compile/test gates supported by the audit runner.
func (s *auditSandbox) RunGo(ctx context.Context, dir string, args []string) (string, string, error) {
	if s == nil || filepath.Clean(dir) != s.sourceDir {
		return "", "", fmt.Errorf("operate: audit sandbox workspace mismatch")
	}
	if len(args) == 0 || args[0] != "test" && args[0] != "vet" {
		return "", "", fmt.Errorf("operate: audit sandbox rejects unsupported Go command")
	}
	goArgs := append([]string{args[0], "-mod=vendor", "-buildvcs=false"}, args[1:]...)
	return s.run(ctx, false, nil, s.sandboxGoPath(), goArgs...)
}

// Build performs the deterministic static audit build. Its output is private
// scratch evidence and is destroyed with the sandbox after the gate returns.
func (s *auditSandbox) Build(ctx context.Context, dir string, out, errOut io.Writer) error {
	if s == nil || filepath.Clean(dir) != s.sourceDir {
		return fmt.Errorf("operate: audit sandbox workspace mismatch")
	}
	_, stdout, stderr, err := s.BuildTarget(ctx, "linux/amd64")
	if stdout != "" && out != nil {
		_, _ = io.WriteString(out, stdout)
	}
	if stderr != "" && errOut != nil {
		_, _ = io.WriteString(errOut, stderr)
	}
	return err
}

// BuildTarget creates one static binary in the sandbox's private output
// directory. Callers must verify the live source snapshot before copying the
// result to durable state.
func (s *auditSandbox) BuildTarget(ctx context.Context, target string) (string, string, string, error) {
	if s == nil {
		return "", "", "", fmt.Errorf("operate: nil audit sandbox")
	}
	if strings.TrimSpace(target) == "" {
		target = "linux/amd64"
	}
	goos, goarch, err := deploy.SplitTarget(target)
	if err != nil {
		return "", "", "", err
	}
	hostOutput := filepath.Join(s.outputDir, "worker")
	if err := os.Remove(hostOutput); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", "", fmt.Errorf("operate: clear stale sandbox build output: %w", err)
	}
	sandboxOutput := filepath.Join(auditSandboxOutput, "worker")
	stdout, stderr, err := s.runWithEnvironment(ctx, false, []auditSandboxMount{{
		source: s.outputDir, destination: auditSandboxOutput, writable: true,
	}}, map[string]string{"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0"}, s.sandboxGoPath(),
		"build", "-mod=vendor", "-buildvcs=false", "-trimpath", "-o", sandboxOutput, "-ldflags", "-s -w", ".")
	if err != nil {
		return "", stdout, stderr, err
	}
	info, err := os.Lstat(hostOutput)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("output is not a regular file")
		}
		return "", stdout, stderr, fmt.Errorf("operate: inspect sandbox build output: %w", err)
	}
	return hostOutput, stdout, stderr, nil
}

func (s *auditSandbox) preflight(ctx context.Context) error {
	_, stderr, err := s.run(ctx, false, nil, s.sandboxGoPath(), "version")
	if err != nil {
		if strings.TrimSpace(stderr) == "" {
			stderr = err.Error()
		}
		return fmt.Errorf("operate: audit sandbox unavailable or cannot enforce namespaces: %s", strings.TrimSpace(stderr))
	}
	return nil
}

func (s *auditSandbox) prepareVendor(ctx context.Context) error {
	goModPath := filepath.Join(s.stageDir, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		return fmt.Errorf("operate: audit sandbox requires worker go.mod: %w", err)
	}
	if _, err := os.Stat(filepath.Join(s.stageDir, "vendor", "modules.txt")); err == nil {
		return rejectSensitiveAuditStage(s.stageDir)
	}

	edits, err := s.readModuleEdits(ctx, goModPath)
	if err != nil {
		return err
	}
	dependencies := make([]auditSandboxMount, 0, len(edits.Replace))
	dependencyByDir := make(map[string]string)
	for _, replacement := range edits.Replace {
		if replacement.New.Version != "" || replacement.New.Path == "" {
			continue
		}
		target := replacement.New.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(s.sourceDir, target)
		}
		_, targetReal, err := realDirectory(target)
		if err != nil {
			return fmt.Errorf("operate: resolve audit local replacement %s: %w", replacement.Old.Path, err)
		}
		if _, err := os.Stat(filepath.Join(targetReal, "go.mod")); err != nil {
			return fmt.Errorf("operate: local replacement %s is not a Go module: %w", replacement.Old.Path, err)
		}
		destination, ok := dependencyByDir[targetReal]
		if !ok {
			destination = fmt.Sprintf("/dependencies/%d", len(dependencyByDir))
			dependencyByDir[targetReal] = destination
			dependencies = append(dependencies, auditSandboxMount{source: targetReal, destination: destination})
		}
		old := replacement.Old.Path
		if replacement.Old.Version != "" {
			old += "@" + replacement.Old.Version
		}
		if err := s.editModuleReplacement(ctx, goModPath, old, destination); err != nil {
			return err
		}
	}

	moduleCache, err := s.auditModuleCache()
	if err != nil {
		return err
	}
	dependencies = append(dependencies, auditSandboxMount{source: moduleCache, destination: "/gomodcache"})
	// Resolve the complete module graph in the disposable stage before
	// vendoring. Tidy recreates a missing go.sum, including the transitive
	// go.mod checksums that `go mod vendor` requires, from the already-populated
	// module cache. This command remains networkless and sandboxed, and any
	// go.mod/go.sum edits land only in the staged copy, preserving the
	// source-bound live worker.
	_, stderr, err := s.run(ctx, true, dependencies, s.sandboxGoPath(), "mod", "tidy")
	if err != nil {
		return fmt.Errorf("operate: resolve offline audit module sums: %s: %w", strings.TrimSpace(stderr), err)
	}
	_, stderr, err = s.run(ctx, true, dependencies, s.sandboxGoPath(), "mod", "vendor")
	if err != nil {
		return fmt.Errorf("operate: prepare offline audit dependencies: %s: %w", strings.TrimSpace(stderr), err)
	}
	return rejectSensitiveAuditStage(s.stageDir)
}

func (s *auditSandbox) readModuleEdits(ctx context.Context, goModPath string) (auditModuleEdit, error) {
	stdout := newBoundedOutput(maxAuditStreamBytes, "go mod edit output")
	stderr := newBoundedOutput(maxAuditStreamBytes, "go mod edit stderr")
	cmd := exec.CommandContext(ctx, s.goBinary, "mod", "edit", "-json", goModPath)
	cmd.Dir = s.stageDir
	cmd.Env = auditHostGoEnvironment(s.goRoot)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return auditModuleEdit{}, fmt.Errorf("operate: inspect worker go.mod: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	var edit auditModuleEdit
	if err := json.Unmarshal([]byte(stdout.String()), &edit); err != nil {
		return auditModuleEdit{}, fmt.Errorf("operate: decode worker go.mod: %w", err)
	}
	return edit, nil
}

func (s *auditSandbox) editModuleReplacement(ctx context.Context, goModPath, old, target string) error {
	cmd := exec.CommandContext(ctx, s.goBinary, "mod", "edit", "-replace="+old+"="+target, goModPath)
	cmd.Dir = s.stageDir
	cmd.Env = auditHostGoEnvironment(s.goRoot)
	stderr := newBoundedOutput(maxAuditStreamBytes, "go mod edit stderr")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("operate: stage local replacement %s: %s: %w", old, strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func (s *auditSandbox) run(ctx context.Context, workspaceWritable bool, extra []auditSandboxMount, name string, args ...string) (string, string, error) {
	return s.runWithEnvironment(ctx, workspaceWritable, extra, nil, name, args...)
}

func (s *auditSandbox) runWithEnvironment(ctx context.Context, workspaceWritable bool, extra []auditSandboxMount, environment map[string]string, name string, args ...string) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	mounts := []auditSandboxMount{
		{source: s.stageDir, destination: auditSandboxWorkspace, writable: workspaceWritable},
		{source: s.cacheDir, destination: auditSandboxCache, writable: true},
		{source: s.tempDir, destination: auditSandboxTemp, writable: true},
	}
	mounts = append(mounts, extra...)
	bwrapArgs, err := s.bwrapArgs(mounts, environment)
	if err != nil {
		return "", "", err
	}
	bwrapArgs = append(bwrapArgs, "--", name)
	bwrapArgs = append(bwrapArgs, args...)
	cmd := exec.CommandContext(ctx, s.bwrap, bwrapArgs...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	stdout := newBoundedOutput(maxAuditStreamBytes, "sandbox command stdout")
	stderr := newBoundedOutput(maxAuditStreamBytes, "sandbox command stderr")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := configureAuditSandboxProcess(cmd); err != nil {
		return "", "", fmt.Errorf("operate: configure audit sandbox process isolation: %w", err)
	}
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return stdout.String(), stderr.String(), err
}

func (s *auditSandbox) bwrapArgs(mounts []auditSandboxMount, environment map[string]string) ([]string, error) {
	args := []string{
		"--die-with-parent", "--unshare-all", "--unshare-user", "--new-session", "--disable-userns", "--cap-drop", "ALL",
		"--clearenv", "--ro-bind", "/usr", "/usr",
	}
	for _, path := range []string{"/lib", "/lib64"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	goRoot := filepath.Clean(s.goRoot)
	if goRoot != "" && !pathWithinRoot("/usr", goRoot) {
		if _, err := os.Stat(goRoot); err != nil {
			return nil, fmt.Errorf("operate: audit Go root unavailable: %w", err)
		}
		args = append(args, "--ro-bind", goRoot, goRoot)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	for _, mount := range mounts {
		if mount.source == "" || mount.destination == "" || !filepath.IsAbs(mount.destination) {
			return nil, fmt.Errorf("operate: invalid audit sandbox mount")
		}
		if _, err := os.Stat(mount.source); err != nil {
			return nil, fmt.Errorf("operate: audit sandbox mount %s: %w", mount.source, err)
		}
		flag := "--ro-bind"
		if mount.writable {
			flag = "--bind"
		}
		args = append(args, flag, mount.source, mount.destination)
	}
	values := map[string]string{
		"PATH": sandboxPath(s.goRoot), "HOME": "/homeless", "USER": "ouvrier", "LOGNAME": "ouvrier",
		"LANG": "C", "TZ": "UTC", "TMPDIR": auditSandboxTemp, "TMP": auditSandboxTemp, "TEMP": auditSandboxTemp,
		"GOTMPDIR": auditSandboxTemp, "GOCACHE": auditSandboxCache, "GOMODCACHE": "/gomodcache",
		"GOENV": "off", "GOWORK": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
		"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0",
	}
	for key, value := range environment {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--setenv", key, values[key])
	}
	args = append(args, "--chdir", auditSandboxWorkspace)
	return args, nil
}

func (s *auditSandbox) sandboxGoPath() string {
	goRoot := filepath.Clean(s.goRoot)
	if goRoot == "" {
		return "/usr/local/go/bin/go"
	}
	return filepath.Join(goRoot, "bin", "go")
}

func sandboxPath(goRoot string) string {
	parts := []string{filepath.Join(filepath.Clean(goRoot), "bin"), "/usr/bin", "/bin"}
	return strings.Join(parts, ":")
}

func (s *auditSandbox) auditModuleCache() (string, error) {
	cmd := exec.Command(s.goBinary, "env", "GOMODCACHE")
	cmd.Env = auditHostGoEnvironment(s.goRoot)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("operate: resolve Go module cache for offline audit: %w", err)
	}
	cache := filepath.Clean(strings.TrimSpace(string(out)))
	info, err := os.Stat(cache)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("operate: offline audit module cache unavailable at %s", cache)
	}
	return cache, nil
}

func auditHostGoEnvironment(goRoot string) []string {
	allowed := map[string]bool{"HOME": true, "GOPATH": true, "GOMODCACHE": true}
	values := map[string]string{
		"PATH": os.Getenv("PATH"), "GOROOT": filepath.Clean(goRoot), "GOENV": "off", "GOWORK": "off", "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0",
	}
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if ok && allowed[name] {
			values[name] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func copyAuditSource(ctx context.Context, source, destination string) error {
	entries := 0
	bytesCopied := int64(0)
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if excludedSourcePath(relSlash, entry.IsDir()) || protectedExternalDriverPath(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxCandidateSourceFiles {
			return fmt.Errorf("operate: audit source exceeds %d entries", maxCandidateSourceFiles)
		}
		target := filepath.Join(destination, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("operate: inspect audit source %s: %w", relSlash, err)
		}
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("operate: stage audit directory %s: %w", relSlash, err)
			}
			return nil
		case info.Mode().IsRegular():
			if info.Size() < 0 || bytesCopied > maxCandidateSourceBytes-info.Size() {
				return fmt.Errorf("operate: audit source exceeds %d bytes", maxCandidateSourceBytes)
			}
			written, err := copyAuditFile(path, target, info)
			if err != nil {
				return fmt.Errorf("operate: stage audit file %s: %w", relSlash, err)
			}
			bytesCopied += written
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("operate: resolve audit source symlink %s: %w", relSlash, err)
			}
			resolved, err = filepath.Abs(resolved)
			if err != nil || !pathWithinRoot(source, resolved) {
				return fmt.Errorf("operate: audit source symlink %s escapes worker", relSlash)
			}
			resolvedRel, err := filepath.Rel(source, resolved)
			if err != nil || isSensitiveWorkerPath(resolvedRel) || excludedSourcePath(filepath.ToSlash(resolvedRel), false) {
				return fmt.Errorf("operate: audit source symlink %s targets protected data", relSlash)
			}
			stagedResolved := filepath.Join(destination, resolvedRel)
			linkTarget, err := filepath.Rel(filepath.Dir(target), stagedResolved)
			if err != nil {
				return fmt.Errorf("operate: stage audit source symlink %s: %w", relSlash, err)
			}
			if err := os.Symlink(linkTarget, target); err != nil {
				return fmt.Errorf("operate: stage audit source symlink %s: %w", relSlash, err)
			}
			return nil
		default:
			return fmt.Errorf("operate: unsupported audit source entry %s", relSlash)
		}
	})
}

func copyAuditFile(source, destination string, expected os.FileInfo) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return 0, err
	}
	in, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != expected.Size() || !os.SameFile(opened, expected) {
		return 0, fmt.Errorf("source changed while opening")
	}
	mode := opened.Mode().Perm() & 0o755
	if mode == 0 {
		mode = 0o600
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(out, io.LimitReader(in, expected.Size()+1))
	closeErr := out.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if written != expected.Size() {
		return 0, fmt.Errorf("source changed while copying")
	}
	after, err := in.Stat()
	if err != nil || after.Size() != opened.Size() || after.ModTime() != opened.ModTime() || !os.SameFile(after, opened) {
		return 0, fmt.Errorf("source changed while copying")
	}
	return written, nil
}

func rejectSensitiveAuditStage(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		if isSensitiveWorkerPath(rel) {
			return fmt.Errorf("operate: audit dependency staging produced sensitive path %s", filepath.ToSlash(rel))
		}
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			if !pathWithinRoot(root, resolved) {
				return fmt.Errorf("operate: audit dependency symlink %s escapes the stage", filepath.ToSlash(rel))
			}
		}
		return nil
	})
}
