// Package deploy implements Ouvrier's deploy engine: building static
// binaries, resolving pip.yaml deploy environments and their env files,
// pre-flight validation, shipping over SSH, and the user-level deployments
// inventory. It deliberately contains no CLI command code so the future
// console can reuse it without importing internal/cli.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBuild is returned when a build cannot proceed (missing pip.yaml,
// invalid flags, or the underlying `go build` failed).
var ErrBuild = errors.New("build error")

// BuildConfig captures the resolved options for `ouvrier build`.
type BuildConfig struct {
	Dir    string
	Output string
	Target string // "GOOS/GOARCH" form, empty = host
	Static bool
}

// GoRunner abstracts the `go build` invocation so tests can stub it out if
// needed. The default implementation runs the system `go` toolchain.
type GoRunner func(ctx context.Context, dir string, env []string, args []string, stdout, stderr io.Writer) error

// DefaultGoRunner runs the system `go` toolchain.
func DefaultGoRunner(ctx context.Context, dir string, env []string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// SplitTarget validates a GOOS/GOARCH cross-compile target.
func SplitTarget(target string) (string, string, error) {
	parts := strings.Split(target, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--target must be in the form GOOS/GOARCH (e.g. linux/amd64), got %q", target)
	}
	return parts[0], parts[1], nil
}

// BuildResult describes the artifacts produced by a successful go build run
// so callers (notably `ouvrier deploy`) can reuse the resolved project name
// and binary path without re-parsing pip.yaml.
type BuildResult struct {
	Dir         string // absolute project directory
	ProjectName string // value of `name:` in pip.yaml
	Output      string // absolute path to the produced binary
}

// Build compiles the project under cfg.Dir and returns the resolved project
// metadata. Keeping a single implementation in one place avoids drift between
// `ouvrier build` and the static build path used by `ouvrier deploy`.
func Build(ctx context.Context, cfg BuildConfig, out, errOut io.Writer, runner GoRunner) (BuildResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: resolve project directory: %w", ErrBuild, err)
	}

	pipPath := filepath.Join(dir, "pip.yaml")
	pipData, err := os.ReadFile(pipPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return BuildResult{}, fmt.Errorf("%w: %s not found; run this command from an Ouvrier project (created by `ouvrier new`)", ErrBuild, pipPath)
		}
		return BuildResult{}, fmt.Errorf("%w: read pip.yaml: %w", ErrBuild, err)
	}

	projectName, err := ParseProjectName(pipData)
	if err != nil {
		return BuildResult{}, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, ".env")); statErr == nil {
		fmt.Fprintln(errOut, "WARN: .env detected in project directory; secrets are never embedded in the binary")
	}

	output := cfg.Output
	if output == "" {
		output = filepath.Join(dir, "bin", projectName)
	} else if !filepath.IsAbs(output) {
		output = filepath.Join(dir, output)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("%w: create output directory: %w", ErrBuild, err)
	}

	env := buildEnv(cfg)

	// -buildvcs=false avoids "error obtaining VCS status" when the project
	// lives outside a git checkout (e.g. fresh `ouvrier new` output) or has
	// no commits yet. We are not embedding VCS metadata anyway.
	args := []string{"build", "-buildvcs=false", "-o", output}
	if cfg.Static {
		args = append(args, "-ldflags", "-s -w")
	}
	args = append(args, ".")

	fmt.Fprintf(out, "building %s -> %s\n", projectName, output)

	if err := runner(ctx, dir, env, args, out, errOut); err != nil {
		return BuildResult{}, fmt.Errorf("%w: go build failed: %w", ErrBuild, err)
	}

	fmt.Fprintf(out, "built %s\n", output)
	return BuildResult{Dir: dir, ProjectName: projectName, Output: output}, nil
}

// StaticBuild compiles a CGO-disabled cross-compiled binary for the project
// under dir. target is a GOOS/GOARCH pair (the deploy `--target` passthrough,
// e.g. linux/arm64); empty means the linux/amd64 default. The returned
// BuildResult carries the absolute output path so the caller (deploy) can scp
// the artifact without re-resolving paths.
func StaticBuild(ctx context.Context, dir, target string, out, errOut io.Writer, runner GoRunner) (BuildResult, error) {
	if strings.TrimSpace(target) == "" {
		target = "linux/amd64"
	}
	if _, _, err := SplitTarget(target); err != nil {
		return BuildResult{}, fmt.Errorf("%w: %w", ErrBuild, err)
	}
	cfg := BuildConfig{
		Dir:    dir,
		Static: true,
		Target: target,
	}
	return Build(ctx, cfg, out, errOut, runner)
}

func buildEnv(cfg BuildConfig) []string {
	env := os.Environ()
	overrides := map[string]string{}
	if cfg.Static {
		overrides["CGO_ENABLED"] = "0"
	}
	if cfg.Target != "" {
		// SplitTarget already validated; ignore error.
		goos, goarch, _ := SplitTarget(cfg.Target)
		overrides["GOOS"] = goos
		overrides["GOARCH"] = goarch
	}
	if len(overrides) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(overrides))
	consumed := make(map[string]bool, len(overrides))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:eq]
		if v, ok := overrides[key]; ok {
			out = append(out, key+"="+v)
			consumed[key] = true
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		if !consumed[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// ParseProjectName extracts the top-level `name:` field from a pip.yaml
// document using a tiny line scanner. We deliberately avoid a YAML dependency.
func ParseProjectName(data []byte) (string, error) {
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Only consider top-level keys (no leading whitespace).
		if line != trimmed {
			continue
		}
		if !strings.HasPrefix(trimmed, "name:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		// Strip an inline comment.
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		value = strings.Trim(value, "\"'")
		if value == "" {
			return "", fmt.Errorf("pip.yaml `name:` is empty")
		}
		if !safeProjectName(value) {
			return "", fmt.Errorf("pip.yaml `name:` %q must be a safe directory name", value)
		}
		return value, nil
	}
	return "", fmt.Errorf("pip.yaml is missing a top-level `name:` field")
}

func safeProjectName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if filepath.Base(name) != name {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
