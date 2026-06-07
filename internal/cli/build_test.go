package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

func TestParsePipNameReadsTopLevelName(t *testing.T) {
	doc := []byte("# header\n\nname: demo\nversion: 0.1.0\n")
	got, err := parsePipName(doc)
	if err != nil {
		t.Fatalf("parsePipName() error = %v", err)
	}
	if got != "demo" {
		t.Fatalf("parsePipName() = %q, want %q", got, "demo")
	}
}

func TestParsePipNameIgnoresNestedKeys(t *testing.T) {
	doc := []byte("deploy:\n  ssh:\n    name: nope\nname: real\n")
	got, err := parsePipName(doc)
	if err != nil {
		t.Fatalf("parsePipName() error = %v", err)
	}
	if got != "real" {
		t.Fatalf("parsePipName() = %q, want %q", got, "real")
	}
}

func TestParsePipNameRejectsUnsafeName(t *testing.T) {
	doc := []byte("name: ../bad\n")
	if _, err := parsePipName(doc); err == nil {
		t.Fatal("parsePipName() error = nil, want unsafe name error")
	}
}

func TestParsePipNameRequiresName(t *testing.T) {
	doc := []byte("version: 0.1.0\n")
	if _, err := parsePipName(doc); err == nil {
		t.Fatal("parsePipName() error = nil, want missing name error")
	}
}

func TestParseBuildFlagsAcceptsAllOptions(t *testing.T) {
	cfg, err := parseBuildFlags([]string{
		"--dir", "/tmp/proj",
		"--output", "out/bin",
		"--target", "linux/amd64",
		"--static",
	})
	if err != nil {
		t.Fatalf("parseBuildFlags() error = %v", err)
	}
	if cfg.Dir != "/tmp/proj" || cfg.Output != "out/bin" || cfg.Target != "linux/amd64" || !cfg.Static {
		t.Fatalf("parseBuildFlags() = %+v", cfg)
	}
}

func TestParseBuildFlagsAcceptsEqualsForm(t *testing.T) {
	cfg, err := parseBuildFlags([]string{"--target=linux/arm64", "--output=./b"})
	if err != nil {
		t.Fatalf("parseBuildFlags() error = %v", err)
	}
	if cfg.Target != "linux/arm64" || cfg.Output != "./b" {
		t.Fatalf("parseBuildFlags() = %+v", cfg)
	}
}

func TestParseBuildFlagsRejectsBadTarget(t *testing.T) {
	if _, err := parseBuildFlags([]string{"--target", "linux"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseBuildFlags() error = %v, want ErrUsage", err)
	}
}

func TestParseBuildFlagsRejectsUnknownArg(t *testing.T) {
	if _, err := parseBuildFlags([]string{"surprise"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseBuildFlags() error = %v, want ErrUsage", err)
	}
}

func TestRunBuildRequiresPipYAML(t *testing.T) {
	dir := t.TempDir()
	err := runBuild(context.Background(), BuildConfig{Dir: dir}, &bytes.Buffer{}, &bytes.Buffer{}, defaultGoRunner)
	if !errors.Is(err, ErrBuild) {
		t.Fatalf("runBuild() error = %v, want ErrBuild", err)
	}
	if !strings.Contains(err.Error(), "pip.yaml") {
		t.Fatalf("runBuild() error = %v, want pip.yaml message", err)
	}
}

func TestRunBuildWarnsOnEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=xyz\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	var out, errOut bytes.Buffer
	runner := func(_ context.Context, _ string, _ []string, _ []string, _, _ io.Writer) error {
		return nil
	}
	if err := runBuild(context.Background(), BuildConfig{Dir: dir}, &out, &errOut, goRunner(runner)); err != nil {
		t.Fatalf("runBuild() error = %v", err)
	}
	if !strings.Contains(errOut.String(), "WARN: .env detected") {
		t.Fatalf("stderr = %q, want .env warning", errOut.String())
	}
	// Value must not appear.
	if strings.Contains(errOut.String(), "xyz") || strings.Contains(out.String(), "xyz") {
		t.Fatalf("env secret leaked into output: stderr=%q stdout=%q", errOut.String(), out.String())
	}
}

func TestRunBuildSetsStaticAndTargetEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	var capturedEnv []string
	var capturedArgs []string
	runner := func(_ context.Context, _ string, env []string, args []string, _, _ io.Writer) error {
		capturedEnv = env
		capturedArgs = args
		return nil
	}
	cfg := BuildConfig{Dir: dir, Static: true, Target: "linux/amd64"}
	if err := runBuild(context.Background(), cfg, &bytes.Buffer{}, &bytes.Buffer{}, goRunner(runner)); err != nil {
		t.Fatalf("runBuild() error = %v", err)
	}
	if !envHas(capturedEnv, "CGO_ENABLED=0") {
		t.Fatalf("env missing CGO_ENABLED=0: %v", filterEnv(capturedEnv))
	}
	if !envHas(capturedEnv, "GOOS=linux") || !envHas(capturedEnv, "GOARCH=amd64") {
		t.Fatalf("env missing GOOS/GOARCH overrides: %v", filterEnv(capturedEnv))
	}
	if !containsAll(capturedArgs, []string{"build", "-o", "-ldflags", "-s -w", "."}) {
		t.Fatalf("go args = %v, missing static ldflags", capturedArgs)
	}
	if strings.Contains(strings.Join(capturedArgs, " "), "./...") {
		t.Fatalf("go args = %v, must build the main package, not ./...", capturedArgs)
	}
}

func TestRunBuildDefaultOutputDerivesFromName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: cooler\n"), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	var capturedArgs []string
	runner := func(_ context.Context, _ string, _ []string, args []string, _, _ io.Writer) error {
		capturedArgs = args
		return nil
	}
	if err := runBuild(context.Background(), BuildConfig{Dir: dir}, &bytes.Buffer{}, &bytes.Buffer{}, goRunner(runner)); err != nil {
		t.Fatalf("runBuild() error = %v", err)
	}
	want := filepath.Join(dir, "bin", "cooler")
	if !containsAll(capturedArgs, []string{"-o", want}) {
		t.Fatalf("go args = %v, want -o %s", capturedArgs, want)
	}
}

func TestRunBuildCompilesScaffoldedProject(t *testing.T) {
	root := repoRoot(t)
	parent := t.TempDir()
	project, err := scaffold.Generate(context.Background(), scaffold.Config{
		Name:         "demo",
		Trigger:      "POST /tickets",
		Model:        "anthropic/claude-sonnet-4-6",
		Dir:          parent,
		FrameworkDir: root,
	})
	if err != nil {
		t.Fatalf("scaffold.Generate() error = %v", err)
	}

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	err = app.Run(context.Background(), []string{"build", "--dir", project.Dir})
	if err != nil {
		t.Fatalf("Run(build) error = %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}

	bin := filepath.Join(project.Dir, "bin", "demo")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("expected binary at %s: %v\nstdout=%s\nstderr=%s", bin, err, out.String(), errOut.String())
	}
}

func TestRunBuildStaticCrossCompilesPureGoSqlite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-compile build in short mode")
	}
	root := repoRoot(t)
	parent := t.TempDir()
	project, err := scaffold.Generate(context.Background(), scaffold.Config{
		Name:         "demo",
		Trigger:      "POST /tickets",
		Model:        "anthropic/claude-sonnet-4-6",
		Dir:          parent,
		FrameworkDir: root,
	})
	if err != nil {
		t.Fatalf("scaffold.Generate() error = %v", err)
	}

	// Pick a cross-target that is NOT the host but is supported by pure-Go
	// modernc.org/sqlite (it ships builds for both linux/amd64 and linux/arm64).
	// If the host already matches one of those, just flip the arch.
	target := "linux/amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		target = "linux/arm64"
	}

	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	output := filepath.Join(project.Dir, "bin", "demo-cross")
	err = app.Run(context.Background(), []string{
		"build",
		"--dir", project.Dir,
		"--static",
		"--target", target,
		"--output", output,
	})
	if err != nil {
		t.Fatalf("Run(build --static --target %s) error = %v\nstdout=%s\nstderr=%s", target, err, out.String(), errOut.String())
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatalf("expected cross-compiled binary at %s: %v", output, err)
	}
}

func TestRunBuildHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"build", "--help"}); err != nil {
		t.Fatalf("Run(build --help) error = %v", err)
	}
	for _, want := range []string{
		"Usage: ouvrier build",
		"--static",
		"--target",
		"--output",
		"--dir",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("build help missing %q in:\n%s", want, out.String())
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", errOut.String())
	}
}

// --- helpers ----------------------------------------------------------------

func envHas(env []string, kv string) bool {
	for _, entry := range env {
		if entry == kv {
			return true
		}
	}
	return false
}

func filterEnv(env []string) []string {
	out := make([]string, 0, 4)
	for _, e := range env {
		if strings.HasPrefix(e, "GOOS=") || strings.HasPrefix(e, "GOARCH=") || strings.HasPrefix(e, "CGO_ENABLED=") {
			out = append(out, e)
		}
	}
	return out
}

func containsAll(haystack, needles []string) bool {
	for _, want := range needles {
		found := false
		for _, got := range haystack {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(wd, "go.mod")); err == nil {
			if strings.HasPrefix(string(data), "module github.com/ArnaudGuiovanna/ouvrier\n") || strings.Contains(string(data), "\nmodule github.com/ArnaudGuiovanna/ouvrier\n") {
				return wd
			}
		}
		next := filepath.Dir(wd)
		if next == wd {
			t.Fatal("could not find repository root with module github.com/ArnaudGuiovanna/ouvrier")
		}
		wd = next
	}
}
