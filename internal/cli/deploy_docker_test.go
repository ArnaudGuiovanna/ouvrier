package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeDocker records every docker invocation as a list of args slices.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	dirs  []string
	fail  func(args []string) error
}

func (f *fakeDocker) Run(_ context.Context, dir string, args []string, _, _ io.Writer) error {
	f.mu.Lock()
	cp := make([]string, len(args))
	copy(cp, args)
	f.calls = append(f.calls, cp)
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	if f.fail != nil {
		return f.fail(args)
	}
	return nil
}

func writeDockerFixture(t *testing.T, withVersion bool) string {
	t.Helper()
	dir := t.TempDir()
	pip := "name: demo\n"
	if withVersion {
		pip = "name: demo\nversion: 1.2.3\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte(pip), 0o644); err != nil {
		t.Fatalf("write pip.yaml: %v", err)
	}
	return dir
}

func TestParseDeployDockerFlagsAcceptsAllOptions(t *testing.T) {
	cfg, err := parseDeployDockerFlags([]string{
		"--image", "registry/demo",
		"--tag", "v2",
		"--push",
		"--force",
		"--dir", "/proj",
	})
	if err != nil {
		t.Fatalf("parseDeployDockerFlags() error = %v", err)
	}
	want := dockerConfig{Dir: "/proj", Image: "registry/demo", Tag: "v2", Push: true, Force: true}
	if cfg != want {
		t.Fatalf("parseDeployDockerFlags() = %+v, want %+v", cfg, want)
	}
}

func TestParseDeployDockerFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseDeployDockerFlags([]string{"--what"}); !errors.Is(err, ErrUsage) {
		t.Fatalf("parseDeployDockerFlags() error = %v, want ErrUsage", err)
	}
}

func TestRunDeployDockerRequiresPipYAML(t *testing.T) {
	dir := t.TempDir()
	err := runDeployDocker(context.Background(), dockerConfig{Dir: dir}, &bytes.Buffer{}, &bytes.Buffer{}, &fakeDocker{})
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("runDeployDocker() error = %v, want ErrDeploy", err)
	}
}

func TestRunDeployDockerHappyPath(t *testing.T) {
	dir := writeDockerFixture(t, true)
	docker := &fakeDocker{}
	var out, errOut bytes.Buffer
	cfg := dockerConfig{Dir: dir}
	if err := runDeployDocker(context.Background(), cfg, &out, &errOut, docker); err != nil {
		t.Fatalf("runDeployDocker() error = %v", err)
	}

	// Dockerfile must exist and contain expected lines.
	dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("Dockerfile not written: %v", err)
	}
	text := string(dockerfile)
	for _, want := range []string{
		"FROM golang:1.25 AS build",
		"WORKDIR /src",
		"COPY . .",
		"RUN CGO_ENABLED=0 go build -o /out/demo .",
		"RUN mkdir -p /runtime && if [ -d skills ]; then cp -a skills /runtime/skills; else mkdir -p /runtime/skills; fi",
		"FROM gcr.io/distroless/static-debian12:nonroot",
		"COPY --from=build /out/demo /demo",
		"COPY --from=build /runtime/skills /skills",
		`ENTRYPOINT ["/demo"]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Dockerfile missing %q in:\n%s", want, text)
		}
	}

	// Exactly one docker call: build with image:tag from pip.yaml.
	if len(docker.calls) != 1 {
		t.Fatalf("docker calls = %d, want 1: %v", len(docker.calls), docker.calls)
	}
	wantArgs := []string{"build", "-t", "demo:1.2.3", "."}
	if !reflect.DeepEqual(docker.calls[0], wantArgs) {
		t.Fatalf("docker build args = %v, want %v", docker.calls[0], wantArgs)
	}
	if docker.dirs[0] != dirAbs(t, dir) {
		t.Fatalf("docker dir = %q, want %q", docker.dirs[0], dirAbs(t, dir))
	}
}

func TestRunDeployDockerDefaultsTagToLatestWithoutVersion(t *testing.T) {
	dir := writeDockerFixture(t, false)
	docker := &fakeDocker{}
	if err := runDeployDocker(context.Background(), dockerConfig{Dir: dir}, &bytes.Buffer{}, &bytes.Buffer{}, docker); err != nil {
		t.Fatalf("runDeployDocker() error = %v", err)
	}
	if !reflect.DeepEqual(docker.calls[0], []string{"build", "-t", "demo:latest", "."}) {
		t.Fatalf("docker build args = %v", docker.calls[0])
	}
}

func TestRunDeployDockerHonoursImageAndTagFlags(t *testing.T) {
	dir := writeDockerFixture(t, true)
	docker := &fakeDocker{}
	cfg := dockerConfig{Dir: dir, Image: "registry.example.com/demo", Tag: "preview"}
	if err := runDeployDocker(context.Background(), cfg, &bytes.Buffer{}, &bytes.Buffer{}, docker); err != nil {
		t.Fatalf("runDeployDocker() error = %v", err)
	}
	if !reflect.DeepEqual(docker.calls[0], []string{"build", "-t", "registry.example.com/demo:preview", "."}) {
		t.Fatalf("docker build args = %v", docker.calls[0])
	}
}

func TestRunDeployDockerPushesWhenAsked(t *testing.T) {
	dir := writeDockerFixture(t, true)
	docker := &fakeDocker{}
	cfg := dockerConfig{Dir: dir, Push: true}
	if err := runDeployDocker(context.Background(), cfg, &bytes.Buffer{}, &bytes.Buffer{}, docker); err != nil {
		t.Fatalf("runDeployDocker() error = %v", err)
	}
	if len(docker.calls) != 2 {
		t.Fatalf("docker calls = %d, want 2: %v", len(docker.calls), docker.calls)
	}
	if !reflect.DeepEqual(docker.calls[1], []string{"push", "demo:1.2.3"}) {
		t.Fatalf("docker push args = %v", docker.calls[1])
	}
}

func TestRunDeployDockerRefusesToOverwriteExistingDockerfile(t *testing.T) {
	dir := writeDockerFixture(t, true)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("# user file\n"), 0o644); err != nil {
		t.Fatalf("write existing Dockerfile: %v", err)
	}
	docker := &fakeDocker{}
	err := runDeployDocker(context.Background(), dockerConfig{Dir: dir}, &bytes.Buffer{}, &bytes.Buffer{}, docker)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("runDeployDocker() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("runDeployDocker() error = %v, want --force hint", err)
	}
	if len(docker.calls) != 0 {
		t.Fatalf("docker should not run; calls = %v", docker.calls)
	}
	// Existing file should be untouched.
	data, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if string(data) != "# user file\n" {
		t.Fatalf("Dockerfile was overwritten: %q", string(data))
	}
}

func TestRunDeployDockerForceOverwritesDockerfile(t *testing.T) {
	dir := writeDockerFixture(t, true)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("# user file\n"), 0o644); err != nil {
		t.Fatalf("write existing Dockerfile: %v", err)
	}
	docker := &fakeDocker{}
	cfg := dockerConfig{Dir: dir, Force: true}
	if err := runDeployDocker(context.Background(), cfg, &bytes.Buffer{}, &bytes.Buffer{}, docker); err != nil {
		t.Fatalf("runDeployDocker() error = %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if !strings.Contains(string(data), "FROM golang:1.25 AS build") {
		t.Fatalf("Dockerfile was not regenerated: %q", string(data))
	}
}

func TestRunDeployDockerSurfacesBuildFailure(t *testing.T) {
	dir := writeDockerFixture(t, true)
	docker := &fakeDocker{fail: func(args []string) error {
		if len(args) > 0 && args[0] == "build" {
			return errors.New("build broke")
		}
		return nil
	}}
	err := runDeployDocker(context.Background(), dockerConfig{Dir: dir}, &bytes.Buffer{}, &bytes.Buffer{}, docker)
	if !errors.Is(err, ErrDeploy) {
		t.Fatalf("runDeployDocker() error = %v, want ErrDeploy", err)
	}
	if !strings.Contains(err.Error(), "docker build") {
		t.Fatalf("runDeployDocker() error = %v, want docker build context", err)
	}
}

func TestRunDeployDockerHelpPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New("dev", WithStreams(nil, &out, &errOut))
	if err := app.Run(context.Background(), []string{"deploy", "docker", "--help"}); err != nil {
		t.Fatalf("Run(deploy docker --help) error = %v", err)
	}
	for _, want := range []string{"Usage: ouvrier deploy docker", "--image", "--tag", "--push", "--force"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("deploy docker help missing %q in:\n%s", want, out.String())
		}
	}
}

func TestRenderDockerfileContainsTwoStages(t *testing.T) {
	body := renderDockerfile("demo")
	if strings.Count(body, "FROM ") != 2 {
		t.Fatalf("expected two FROM lines, got:\n%s", body)
	}
	if !strings.Contains(body, "gcr.io/distroless/static-debian12:nonroot") {
		t.Fatalf("expected distroless base, got:\n%s", body)
	}
}

// dirAbs resolves a directory the same way runDeployDocker does so test
// comparisons don't depend on the host's tempdir layout.
func dirAbs(t *testing.T, dir string) string {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", dir, err)
	}
	return abs
}
