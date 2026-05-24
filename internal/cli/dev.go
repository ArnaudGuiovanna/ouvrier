package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrDev is returned when `ouvrier dev` cannot proceed.
var ErrDev = errors.New("dev error")

// DevConfig captures the resolved options for `ouvrier dev`.
type DevConfig struct {
	Dir  string
	Addr string
}

// devRunner abstracts the `go run .` invocation so tests can stub it out.
type devRunner func(ctx context.Context, dir string, env []string, stdout, stderr io.Writer) error

func defaultDevRunner(ctx context.Context, dir string, env []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "go", "run", ".")
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// On unix, send the same signal we received to the child. WaitDelay
	// ensures the child gets a chance to exit cleanly after SIGINT/SIGTERM.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	return cmd.Run()
}

func (app *App) runDevCommand(ctx context.Context, args []string) error {
	if hasHelpFlag(args) {
		printDevHelp(app.out)
		return nil
	}
	cfg, err := parseDevFlags(args)
	if err != nil {
		return err
	}
	return runDev(ctx, cfg, app.out, app.errOut, defaultDevRunner)
}

func parseDevFlags(args []string) (DevConfig, error) {
	flags := flag.NewFlagSet("dev", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", ".", "project directory")
	addr := flags.String("addr", "", "override the worker listen address via PIP_ADDR")
	if err := flags.Parse(args); err != nil {
		return DevConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return DevConfig{}, fmt.Errorf("%w: dev does not accept positional arguments", ErrUsage)
	}
	return DevConfig{
		Dir:  *dir,
		Addr: strings.TrimSpace(*addr),
	}, nil
}

func runDev(ctx context.Context, cfg DevConfig, out, errOut io.Writer, runner devRunner) error {
	if ctx == nil {
		ctx = context.Background()
	}

	dir, err := filepath.Abs(strings.TrimSpace(cfg.Dir))
	if err != nil {
		return fmt.Errorf("%w: resolve --dir: %w", ErrDev, err)
	}

	pipPath := filepath.Join(dir, "pip.yaml")
	if _, statErr := os.Stat(pipPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: %s not found; run this command from an Ouvrier project (created by `ouvrier new`)", ErrDev, pipPath)
		}
		return fmt.Errorf("%w: stat pip.yaml: %w", ErrDev, statErr)
	}

	env := devEnv(cfg)

	// Wire SIGINT/SIGTERM into a cancellable context so the runner can stop
	// the child process gracefully. The signal package writes to the channel
	// before we cancel; runners interpret cancellation via the supplied ctx.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	done := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(errOut, "\nouvrier dev: shutting down worker")
			cancel()
		case <-done:
		case <-runCtx.Done():
		}
	}()

	fmt.Fprintf(out, "ouvrier dev: running `go run .` in %s\n", dir)
	if cfg.Addr != "" {
		fmt.Fprintf(out, "ouvrier dev: PIP_ADDR=%s\n", cfg.Addr)
	}
	fmt.Fprintln(out, "ouvrier dev: hot reload is NOT implemented in v0.1; restart this command after edits to main.go, tools/, or skills/.")

	runErr := runner(runCtx, dir, env, out, errOut)
	close(done)

	if runErr != nil {
		// If the user asked us to shut down, that is success.
		if errors.Is(runErr, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
			return nil
		}
		// An ExitError from the child surfaces with its exit code preserved
		// in stderr; bubble it up as a dev error so the parent sees a
		// non-zero exit code while keeping the message specific.
		return fmt.Errorf("%w: go run failed: %w", ErrDev, runErr)
	}
	return nil
}

func devEnv(cfg DevConfig) []string {
	env := os.Environ()
	if cfg.Addr == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	consumed := false
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			out = append(out, kv)
			continue
		}
		if kv[:eq] == "PIP_ADDR" {
			out = append(out, "PIP_ADDR="+cfg.Addr)
			consumed = true
			continue
		}
		out = append(out, kv)
	}
	if !consumed {
		out = append(out, "PIP_ADDR="+cfg.Addr)
	}
	return out
}
