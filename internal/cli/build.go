package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrBuild is returned when the build command cannot proceed (missing pip.yaml,
// invalid flags, or the underlying `go build` failed).
var ErrBuild = errors.New("build error")

// BuildConfig captures the resolved options for `ouvrier build`.
type BuildConfig struct {
	Dir    string
	Output string
	Target string // "GOOS/GOARCH" form, empty = host
	Static bool
}

// goRunner abstracts the `go build` invocation so tests can stub it out if
// needed. The default implementation runs the system `go` toolchain.
type goRunner func(ctx context.Context, dir string, env []string, args []string, stdout, stderr io.Writer) error

func defaultGoRunner(ctx context.Context, dir string, env []string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func (app *App) runBuildCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printBuildHelp(app.out)
		return nil
	}

	cfg, err := parseBuildFlags(args)
	if err != nil {
		return err
	}
	return runBuild(ctx, cfg, app.out, app.errOut, defaultGoRunner)
}

func parseBuildFlags(args []string) (BuildConfig, error) {
	cfg := BuildConfig{Dir: "."}
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--static":
			cfg.Static = true
			i++
		case arg == "--target":
			if i+1 >= len(args) {
				return BuildConfig{}, fmt.Errorf("%w: --target requires a value like linux/amd64", ErrUsage)
			}
			cfg.Target = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--target="):
			cfg.Target = strings.TrimPrefix(arg, "--target=")
			i++
		case arg == "--output", arg == "-o":
			if i+1 >= len(args) {
				return BuildConfig{}, fmt.Errorf("%w: --output requires a path", ErrUsage)
			}
			cfg.Output = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--output="):
			cfg.Output = strings.TrimPrefix(arg, "--output=")
			i++
		case arg == "--dir":
			if i+1 >= len(args) {
				return BuildConfig{}, fmt.Errorf("%w: --dir requires a path", ErrUsage)
			}
			cfg.Dir = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--dir="):
			cfg.Dir = strings.TrimPrefix(arg, "--dir=")
			i++
		default:
			return BuildConfig{}, fmt.Errorf("%w: build does not accept argument %q", ErrUsage, arg)
		}
	}
	if cfg.Dir == "" {
		cfg.Dir = "."
	}
	if cfg.Target != "" {
		if _, _, err := splitTarget(cfg.Target); err != nil {
			return BuildConfig{}, err
		}
	}
	return cfg, nil
}

func splitTarget(target string) (string, string, error) {
	parts := strings.Split(target, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: --target must be in the form GOOS/GOARCH (e.g. linux/amd64), got %q", ErrUsage, target)
	}
	return parts[0], parts[1], nil
}

func runBuild(ctx context.Context, cfg BuildConfig, out, errOut io.Writer, runner goRunner) error {
	_, err := runBuildResolved(ctx, cfg, out, errOut, runner)
	return err
}

// buildResult describes the artifacts produced by a successful go build run
// so callers (notably `ouvrier deploy`) can reuse the resolved project name
// and binary path without re-parsing pip.yaml.
type buildResult struct {
	Dir         string // absolute project directory
	ProjectName string // value of `name:` in pip.yaml
	Output      string // absolute path to the produced binary
}

// runBuildResolved performs the same work as runBuild but also returns the
// resolved project metadata to callers. Keeping a single implementation in one
// place avoids drift between `ouvrier build` and the static build path used by
// `ouvrier deploy`.
func runBuildResolved(ctx context.Context, cfg BuildConfig, out, errOut io.Writer, runner goRunner) (buildResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return buildResult{}, fmt.Errorf("%w: resolve project directory: %w", ErrBuild, err)
	}

	pipPath := filepath.Join(dir, "pip.yaml")
	pipData, err := os.ReadFile(pipPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return buildResult{}, fmt.Errorf("%w: %s not found; run this command from an Ouvrier project (created by `ouvrier new`)", ErrBuild, pipPath)
		}
		return buildResult{}, fmt.Errorf("%w: read pip.yaml: %w", ErrBuild, err)
	}

	projectName, err := parsePipName(pipData)
	if err != nil {
		return buildResult{}, fmt.Errorf("%w: %w", ErrBuild, err)
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
		return buildResult{}, fmt.Errorf("%w: create output directory: %w", ErrBuild, err)
	}

	env := buildEnv(cfg)

	// -buildvcs=false avoids "error obtaining VCS status" when the project
	// lives outside a git checkout (e.g. fresh `ouvrier new` output) or has
	// no commits yet. We are not embedding VCS metadata anyway.
	args := []string{"build", "-buildvcs=false", "-o", output}
	if cfg.Static {
		args = append(args, "-ldflags", "-s -w")
	}
	args = append(args, "./...")

	fmt.Fprintf(out, "building %s -> %s\n", projectName, output)

	if err := runner(ctx, dir, env, args, out, errOut); err != nil {
		return buildResult{}, fmt.Errorf("%w: go build failed: %w", ErrBuild, err)
	}

	fmt.Fprintf(out, "built %s\n", output)
	return buildResult{Dir: dir, ProjectName: projectName, Output: output}, nil
}

// staticBuildForDeploy compiles a CGO-disabled, linux/amd64 binary for the
// project under dir. The returned buildResult carries the absolute output
// path so the caller (deploy) can scp the artifact without re-resolving paths.
func staticBuildForDeploy(ctx context.Context, dir string, out, errOut io.Writer, runner goRunner) (buildResult, error) {
	cfg := BuildConfig{
		Dir:    dir,
		Static: true,
		Target: "linux/amd64",
	}
	return runBuildResolved(ctx, cfg, out, errOut, runner)
}

func buildEnv(cfg BuildConfig) []string {
	env := os.Environ()
	overrides := map[string]string{}
	if cfg.Static {
		overrides["CGO_ENABLED"] = "0"
	}
	if cfg.Target != "" {
		// splitTarget already validated; ignore error.
		goos, goarch, _ := splitTarget(cfg.Target)
		overrides["GOOS"] = goos
		overrides["GOARCH"] = goarch
	} else {
		// For host builds we leave GOOS/GOARCH untouched so users keep their
		// existing environment behaviour. Record host arch in case it helps
		// debugging via logs in the future.
		_ = runtime.GOOS
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

// parsePipName extracts the top-level `name:` field from a pip.yaml document
// using a tiny line scanner. We deliberately avoid a YAML dependency.
func parsePipName(data []byte) (string, error) {
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
