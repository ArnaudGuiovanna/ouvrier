package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDevFlagsAcceptsAddrAndDir(t *testing.T) {
	cfg, err := parseDevFlags([]string{"--dir", "/tmp/proj", "--addr", ":9090"})
	if err != nil {
		t.Fatalf("parseDevFlags() error = %v", err)
	}
	if cfg.Dir != "/tmp/proj" || cfg.Addr != ":9090" {
		t.Fatalf("parseDevFlags() = %+v", cfg)
	}
}

func TestParseDevFlagsRejectsPositional(t *testing.T) {
	if _, err := parseDevFlags([]string{"unexpected"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseDevFlags() error = %v, want ErrUsage", err)
	}
}

func TestRunDevRequiresPipYAML(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	stub := func(_ context.Context, _ string, _ []string, _, _ io.Writer) error { return nil }
	err := runDev(context.Background(), DevConfig{Dir: dir}, &out, &errOut, devRunner(stub))
	if !errors.Is(err, ErrDev) {
		t.Fatalf("runDev() error = %v, want ErrDev", err)
	}
	if !strings.Contains(err.Error(), "pip.yaml") {
		t.Fatalf("runDev() error = %v, want pip.yaml mention", err)
	}
}

func TestRunDevInvokesRunnerWithDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}

	var capturedDir string
	var capturedEnv []string
	stub := func(_ context.Context, runDir string, env []string, _, _ io.Writer) error {
		capturedDir = runDir
		capturedEnv = env
		return nil
	}

	var out, errOut bytes.Buffer
	err := runDev(context.Background(), DevConfig{Dir: dir, Addr: ":9090", NoReload: true}, &out, &errOut, devRunner(stub))
	if err != nil {
		t.Fatalf("runDev() error = %v\nstderr=%s", err, errOut.String())
	}
	abs, _ := filepath.Abs(dir)
	if capturedDir != abs {
		t.Fatalf("dev runner dir = %q, want %q", capturedDir, abs)
	}
	if !envHas(capturedEnv, "PIP_ADDR=:9090") {
		t.Fatalf("dev runner env missing PIP_ADDR; got %v", filterDevEnv(capturedEnv))
	}
	if !strings.Contains(out.String(), "hot reload disabled") {
		t.Fatalf("dev did not log --no-reload notice; got:\n%s", out.String())
	}
}

func TestRunDevReturnsErrDevOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	stub := func(_ context.Context, _ string, _ []string, _, _ io.Writer) error {
		return errors.New("boom")
	}
	var out, errOut bytes.Buffer
	err := runDev(context.Background(), DevConfig{Dir: dir, NoReload: true}, &out, &errOut, devRunner(stub))
	if !errors.Is(err, ErrDev) {
		t.Fatalf("runDev() error = %v, want ErrDev", err)
	}
}

func TestRunDevTreatsContextCancelAsClean(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	stub := func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately so stub returns context.Canceled.

	var out, errOut bytes.Buffer
	if err := runDev(ctx, DevConfig{Dir: dir, NoReload: true}, &out, &errOut, devRunner(stub)); err != nil {
		t.Fatalf("runDev() error = %v, want nil after context cancellation", err)
	}
}

func TestRunDevHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"dev", "--help"}); err != nil {
		t.Fatalf("Run(dev --help) error = %v", err)
	}
	if !strings.Contains(out.String(), "Usage: ouvrier dev") {
		t.Fatalf("dev help missing usage; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Hot reload") {
		t.Fatalf("dev help missing hot reload limitation; got:\n%s", out.String())
	}
}

func TestParseDevFlagsAcceptsNoReload(t *testing.T) {
	cfg, err := parseDevFlags([]string{"--no-reload"})
	if err != nil {
		t.Fatalf("parseDevFlags() error = %v", err)
	}
	if !cfg.NoReload {
		t.Fatalf("parseDevFlags() NoReload = false, want true")
	}
}

// TestRunDevLoopRestartsOnTrigger drives the watch state machine with a
// stubbed runner: the worker blocks until its per-run context is cancelled.
// A trigger must stop the current run and start a fresh one.
func TestRunDevLoopRestartsOnTrigger(t *testing.T) {
	starts := make(chan struct{}, 8)
	stub := func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		starts <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}

	triggers := make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())

	var out, errOut bytes.Buffer
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runDevLoop(runCtx, "/proj", nil, &out, &errOut, devRunner(stub), triggers)
	}()

	// First start.
	waitForStart(t, starts)

	// A trigger should restart the worker.
	triggers <- struct{}{}
	waitForStart(t, starts)

	// Second trigger restarts again.
	triggers <- struct{}{}
	waitForStart(t, starts)

	cancel()
	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("runDevLoop() error = %v, want nil after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDevLoop did not return after cancel")
	}

	if !strings.Contains(out.String(), "reloading") {
		t.Fatalf("expected reloading output; got:\n%s", out.String())
	}
}

// TestRunDevLoopKeepsWatchingAfterFailure ensures a build/start failure does
// not exit the loop: the worker exits with an error, the loop logs it and
// waits for the next change to retry.
func TestRunDevLoopKeepsWatchingAfterFailure(t *testing.T) {
	starts := make(chan struct{}, 8)
	var calls int
	stub := func(ctx context.Context, _ string, _ []string, _, _ io.Writer) error {
		calls++
		starts <- struct{}{}
		if calls == 1 {
			return errors.New("build failed")
		}
		<-ctx.Done()
		return ctx.Err()
	}

	triggers := make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())

	var out, errOut bytes.Buffer
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- runDevLoop(runCtx, "/proj", nil, &out, &errOut, devRunner(stub), triggers)
	}()

	// First start fails immediately; loop should not exit.
	waitForStart(t, starts)

	// A change should retry the worker.
	triggers <- struct{}{}
	waitForStart(t, starts)

	cancel()
	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("runDevLoop() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runDevLoop did not return after cancel")
	}

	if !strings.Contains(errOut.String(), "build failed") {
		t.Fatalf("expected failure logged to stderr; got:\n%s", errOut.String())
	}
}

// TestRunDevNoReloadDoesNotRestart confirms --no-reload runs the worker exactly
// once even if the runner returns (no watch loop, no restart).
func TestRunDevNoReloadDoesNotRestart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	var calls int
	stub := func(_ context.Context, _ string, _ []string, _, _ io.Writer) error {
		calls++
		return nil
	}
	var out, errOut bytes.Buffer
	if err := runDev(context.Background(), DevConfig{Dir: dir, NoReload: true}, &out, &errOut, devRunner(stub)); err != nil {
		t.Fatalf("runDev() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner called %d times, want 1 (no restart with --no-reload)", calls)
	}
}

// TestWatchProjectDetectsGoFileChange verifies the polling watcher emits on a
// real file change and ignores files under .ouvrier/.
func TestWatchProjectDetectsGoFileChange(t *testing.T) {
	dir := t.TempDir()
	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// Build artifacts under .ouvrier/ must be ignored.
	ouvrierDir := filepath.Join(dir, ".ouvrier")
	if err := os.MkdirAll(ouvrierDir, 0o755); err != nil {
		t.Fatalf("mkdir .ouvrier: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	triggers := watchProject(ctx, dir, 20*time.Millisecond, 20*time.Millisecond)

	// Touching a file under .ouvrier/ should NOT trigger.
	time.Sleep(40 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(ouvrierDir, "state.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write state.db: %v", err)
	}
	select {
	case <-triggers:
		t.Fatal("watchProject triggered on ignored .ouvrier/ change")
	case <-time.After(150 * time.Millisecond):
	}

	// Modifying main.go should trigger.
	if err := os.WriteFile(mainGo, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("rewrite main.go: %v", err)
	}
	select {
	case <-triggers:
	case <-time.After(2 * time.Second):
		t.Fatal("watchProject did not trigger on main.go change")
	}
}

func TestParseDevFlagsAcceptsNoDotenv(t *testing.T) {
	cfg, err := parseDevFlags([]string{"--no-dotenv"})
	if err != nil {
		t.Fatalf("parseDevFlags() error = %v", err)
	}
	if !cfg.NoDotenv {
		t.Fatalf("parseDevFlags() NoDotenv = false, want true")
	}
}

func TestRunDevLoadsDotenvIntoChildEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OUVRIER_DEV_DOTENV_ONLY=fromfile\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	var capturedEnv []string
	stub := func(_ context.Context, _ string, env []string, _, _ io.Writer) error {
		capturedEnv = env
		return nil
	}
	var out, errOut bytes.Buffer
	if err := runDev(context.Background(), DevConfig{Dir: dir, NoReload: true}, &out, &errOut, devRunner(stub)); err != nil {
		t.Fatalf("runDev() error = %v", err)
	}
	if !envHas(capturedEnv, "OUVRIER_DEV_DOTENV_ONLY=fromfile") {
		t.Fatalf("child env missing .env value; got %v", capturedEnv)
	}
}

func TestRunDevProcessEnvWinsOverDotenv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	const key = "OUVRIER_DEV_PRECEDENCE_TEST"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(key+"=fromfile\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Setenv(key, "fromprocess")

	var capturedEnv []string
	stub := func(_ context.Context, _ string, env []string, _, _ io.Writer) error {
		capturedEnv = env
		return nil
	}
	var out, errOut bytes.Buffer
	if err := runDev(context.Background(), DevConfig{Dir: dir, NoReload: true}, &out, &errOut, devRunner(stub)); err != nil {
		t.Fatalf("runDev() error = %v", err)
	}
	if !envHas(capturedEnv, key+"=fromprocess") {
		t.Fatalf("process env should win; got %v", capturedEnv)
	}
	if envHas(capturedEnv, key+"=fromfile") {
		t.Fatalf(".env overrode process env; got %v", capturedEnv)
	}
}

func TestRunDevNoDotenvSkipsLoading(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OUVRIER_DEV_SKIPPED=fromfile\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	var capturedEnv []string
	stub := func(_ context.Context, _ string, env []string, _, _ io.Writer) error {
		capturedEnv = env
		return nil
	}
	var out, errOut bytes.Buffer
	if err := runDev(context.Background(), DevConfig{Dir: dir, NoReload: true, NoDotenv: true}, &out, &errOut, devRunner(stub)); err != nil {
		t.Fatalf("runDev() error = %v", err)
	}
	if envHas(capturedEnv, "OUVRIER_DEV_SKIPPED=fromfile") {
		t.Fatalf("--no-dotenv should skip .env loading; got %v", capturedEnv)
	}
}

func TestRunDevDotenvValuesNotPrinted(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	const secret = "supersecretvalue12345"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MY_SECRET="+secret+"\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	stub := func(_ context.Context, _ string, _ []string, _, _ io.Writer) error { return nil }
	var out, errOut bytes.Buffer
	if err := runDev(context.Background(), DevConfig{Dir: dir, NoReload: true}, &out, &errOut, devRunner(stub)); err != nil {
		t.Fatalf("runDev() error = %v", err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
		t.Fatalf("dev printed a .env secret value:\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
}

func waitForStart(t *testing.T, starts <-chan struct{}) {
	t.Helper()
	select {
	case <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start in time")
	}
}

func filterDevEnv(env []string) []string {
	out := make([]string, 0, 2)
	for _, e := range env {
		if strings.HasPrefix(e, "PIP_ADDR=") {
			out = append(out, e)
		}
	}
	return out
}
