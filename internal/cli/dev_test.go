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
	err := runDev(context.Background(), DevConfig{Dir: dir, Addr: ":9090"}, &out, &errOut, devRunner(stub))
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
	if !strings.Contains(out.String(), "hot reload is NOT implemented") {
		t.Fatalf("dev did not log hot-reload limitation; got:\n%s", out.String())
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
	err := runDev(context.Background(), DevConfig{Dir: dir}, &out, &errOut, devRunner(stub))
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
	if err := runDev(ctx, DevConfig{Dir: dir}, &out, &errOut, devRunner(stub)); err != nil {
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

func filterDevEnv(env []string) []string {
	out := make([]string, 0, 2)
	for _, e := range env {
		if strings.HasPrefix(e, "PIP_ADDR=") {
			out = append(out, e)
		}
	}
	return out
}
