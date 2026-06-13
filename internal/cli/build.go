package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

// The build engine lives in internal/deploy so the deploy engine (and the
// future console) can compile workers without importing CLI command code.
// These aliases keep internal/cli call sites and tests unchanged.
type (
	// BuildConfig captures the resolved options for `ouvrier build`.
	BuildConfig = deploy.BuildConfig
	// goRunner abstracts the `go build` invocation so tests can stub it out.
	goRunner = deploy.GoRunner
)

// ErrBuild is returned when the build command cannot proceed (missing pip.yaml,
// invalid flags, or the underlying `go build` failed).
var ErrBuild = deploy.ErrBuild

// defaultGoRunner runs the system `go` toolchain.
var defaultGoRunner goRunner = deploy.DefaultGoRunner

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
		if _, _, err := deploy.SplitTarget(cfg.Target); err != nil {
			return BuildConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
		}
	}
	return cfg, nil
}

func runBuild(ctx context.Context, cfg BuildConfig, out, errOut io.Writer, runner goRunner) error {
	_, err := deploy.Build(ctx, cfg, out, errOut, runner)
	return err
}

// parsePipName extracts the top-level `name:` field from a pip.yaml document.
// Kept as a wrapper so CLI call sites read naturally.
func parsePipName(data []byte) (string, error) {
	return deploy.ParseProjectName(data)
}
