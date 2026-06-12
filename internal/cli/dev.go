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
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
)

// ErrDev is returned when `ouvrier dev` cannot proceed.
var ErrDev = errors.New("dev error")

// devPollInterval is how often the file watcher samples mod-times.
const devPollInterval = 300 * time.Millisecond

// devDebounce is how long the watcher waits for the filesystem to settle
// after the first detected change before triggering a reload. Editors often
// touch several files in quick succession; debouncing collapses them.
const devDebounce = 200 * time.Millisecond

// DevConfig captures the resolved options for `ouvrier dev`.
type DevConfig struct {
	Dir      string
	Addr     string
	NoReload bool
	NoDotenv bool
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
	addr := flags.String("addr", "", "override the worker listen address via OUVRIER_ADDR")
	noReload := flags.Bool("no-reload", false, "disable hot reload; run `go run .` once")
	noDotenv := flags.Bool("no-dotenv", false, "do not auto-load a local .env into the worker environment")
	if err := flags.Parse(args); err != nil {
		return DevConfig{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return DevConfig{}, fmt.Errorf("%w: dev does not accept positional arguments", ErrUsage)
	}
	return DevConfig{
		Dir:      *dir,
		Addr:     strings.TrimSpace(*addr),
		NoReload: *noReload,
		NoDotenv: *noDotenv,
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

	env := devEnv(cfg, dir, errOut)

	// Wire SIGINT/SIGTERM into a cancellable context so the runner can stop
	// the child process gracefully. Cancellation flows down to each child via
	// the supplied ctx.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(errOut, "\nouvrier dev: shutting down worker")
			cancel()
		case <-runCtx.Done():
		}
	}()

	if cfg.NoReload {
		fmt.Fprintf(out, "ouvrier dev: running `go run .` in %s\n", dir)
		if cfg.Addr != "" {
			fmt.Fprintf(out, "ouvrier dev: %s=%s\n", envnames.Addr, cfg.Addr)
		}
		fmt.Fprintln(out, "ouvrier dev: hot reload disabled (--no-reload); restart this command after edits to main.go, tools/, or skills/.")
		return finishDevRun(runner(runCtx, dir, env, out, errOut), runCtx)
	}

	fmt.Fprintf(out, "ouvrier dev: watching %s for changes (hot reload enabled)\n", dir)
	if cfg.Addr != "" {
		fmt.Fprintf(out, "ouvrier dev: %s=%s\n", envnames.Addr, cfg.Addr)
	}

	// Start the file watcher; it emits one signal per debounced change burst.
	triggers := watchProject(runCtx, dir, devPollInterval, devDebounce)
	return runDevLoop(runCtx, dir, env, out, errOut, runner, triggers)
}

// runDevLoop runs the worker, restarting it whenever a trigger arrives on the
// triggers channel. It returns nil when runCtx is cancelled (a clean shutdown)
// and only returns an error for conditions that should abort the whole loop.
// Build/start failures are logged and the loop keeps watching.
func runDevLoop(runCtx context.Context, dir string, env []string, out, errOut io.Writer, runner devRunner, triggers <-chan struct{}) error {
	for {
		// Start the child in its own cancellable context so a trigger can
		// stop just this run without tearing down the watch loop.
		childCtx, stop := context.WithCancel(runCtx)
		runResult := make(chan error, 1)
		go func() {
			runResult <- runner(childCtx, dir, env, out, errOut)
		}()

		select {
		case <-runCtx.Done():
			stop()
			<-runResult
			return nil

		case err := <-runResult:
			// The child exited on its own (crash, build failure, or clean
			// exit). Keep watching so the next save can recover.
			stop()
			if err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(errOut, "ouvrier dev: worker exited with error: %v\n", err)
			} else {
				fmt.Fprintln(out, "ouvrier dev: worker exited; waiting for changes")
			}
			// Block until a change (or shutdown) before restarting so we do
			// not hot-loop on a process that fails immediately.
			select {
			case <-runCtx.Done():
				return nil
			case _, ok := <-triggers:
				if !ok {
					return nil
				}
				fmt.Fprintln(out, "ouvrier dev: change detected, reloading...")
			}

		case _, ok := <-triggers:
			if !ok {
				stop()
				<-runResult
				return nil
			}
			fmt.Fprintln(out, "ouvrier dev: change detected, reloading...")
			stop()
			<-runResult
		}
	}
}

// finishDevRun translates a single runner result into a dev command result.
func finishDevRun(runErr error, runCtx context.Context) error {
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
			return nil
		}
		return fmt.Errorf("%w: go run failed: %w", ErrDev, runErr)
	}
	return nil
}

// watchProject polls the project sources under dir and emits a value on the
// returned channel for each debounced burst of changes. It stops when ctx is
// cancelled (closing the channel). It uses os.Stat mod-times only, so it needs
// no third-party file-notification dependency.
func watchProject(ctx context.Context, dir string, interval, debounce time.Duration) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		defer close(out)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		prev := snapshotProject(dir)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			cur := snapshotProject(dir)
			if snapshotsEqual(prev, cur) {
				continue
			}

			// Debounce: keep sampling until the tree settles so a multi-file
			// save collapses into a single reload.
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(debounce):
				}
				next := snapshotProject(dir)
				if snapshotsEqual(cur, next) {
					cur = next
					break
				}
				cur = next
			}

			prev = cur
			select {
			case <-ctx.Done():
				return
			case out <- struct{}{}:
			}
		}
	}()
	return out
}

// snapshotProject records the mod-times of the watched source files under dir.
func snapshotProject(dir string) map[string]time.Time {
	snap := make(map[string]time.Time)
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if devShouldSkipDir(dir, path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !devWatchedFile(path) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		snap[path] = info.ModTime()
		return nil
	})
	return snap
}

func snapshotsEqual(a, b map[string]time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !va.Equal(vb) {
			return false
		}
	}
	return true
}

// devShouldSkipDir reports whether a directory should be excluded from the
// watch: dotfiles/dirs (including .ouvrier and .git), vendor, and node_modules.
func devShouldSkipDir(root, path, name string) bool {
	if path == root {
		return false
	}
	if name == "vendor" || name == "node_modules" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	return false
}

// devWatchedFile reports whether a file path is a project source we watch:
// any *.go file, files under tools/ or skills/, and pip.yaml.
func devWatchedFile(path string) bool {
	base := filepath.Base(path)
	if base == "pip.yaml" {
		return true
	}
	if strings.HasSuffix(base, ".go") {
		return true
	}
	// Files under tools/ or skills/ (e.g. SKILL.md, scripts) are sources too.
	// Inspect only the directory components, not the file name itself.
	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	for _, p := range parts {
		if p == "tools" || p == "skills" {
			return true
		}
	}
	return false
}

// devEnv builds the child process environment for `ouvrier dev`. It starts
// from the real process environment, optionally merges a local .env (dev-only;
// the process environment always wins), and finally applies the --addr
// override via OUVRIER_ADDR. .env values are never logged.
func devEnv(cfg DevConfig, dir string, errOut io.Writer) []string {
	env := os.Environ()
	if !cfg.NoDotenv {
		dotenv, err := loadDotenvFile(filepath.Join(dir, ".env"))
		if err != nil {
			// Report the failure but never echo file contents or values.
			fmt.Fprintf(errOut, "ouvrier dev: ignoring unreadable .env: %v\n", err)
		} else if len(dotenv) > 0 {
			env = mergeDotenvEnv(env, dotenv)
			fmt.Fprintf(errOut, "ouvrier dev: loaded %d variable(s) from .env\n", len(dotenv))
		}
	}
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
		if kv[:eq] == envnames.Addr {
			out = append(out, envnames.Addr+"="+cfg.Addr)
			consumed = true
			continue
		}
		out = append(out, kv)
	}
	if !consumed {
		out = append(out, envnames.Addr+"="+cfg.Addr)
	}
	return out
}
