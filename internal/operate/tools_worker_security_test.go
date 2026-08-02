package operate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

func TestBuildWorkerIgnoresModelOverrideAndHonorsOperatorOption(t *testing.T) {
	dir := writeWorkerFixture(t)
	buildCalls := 0
	harness, err := NewHarness(Options{
		Dir: dir,
		Builder: BuildCoordinator{GoRun: func(_ context.Context, _ string, _ []string, args []string, _, _ io.Writer) error {
			buildCalls++
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-o" {
					return os.WriteFile(args[i+1], []byte("worker binary"), 0o755)
				}
			}
			return nil
		}},
	})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	session, workspace, err := harness.Start(context.Background(), dir, "", "manual", "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	input := map[string]any{"allow_failed": true}
	_, err = toolBuildWorker(context.Background(), ToolEnv{
		Harness: harness, Session: session, Workspace: &workspace,
	}, input)
	if err == nil || !strings.Contains(err.Error(), "build requires passing audit") {
		t.Fatalf("model override build error = %v, want audit gate", err)
	}
	if buildCalls != 0 {
		t.Fatalf("build calls after model override = %d, want 0", buildCalls)
	}

	result, err := toolBuildWorker(context.Background(), ToolEnv{
		Harness: harness, Session: session, Workspace: &workspace,
		Options: RuntimeOptions{AllowFail: true},
	}, map[string]any{})
	if err != nil {
		t.Fatalf("operator override build error = %v", err)
	}
	if buildCalls != 1 {
		t.Fatalf("build calls after operator override = %d, want 1", buildCalls)
	}
	if result.Data["binary_path"] == "" {
		t.Fatalf("build result = %+v, want binary path", result)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(session.BuildPath), "build.json")); err != nil {
		t.Fatalf("build artifact missing: %v", err)
	}
}

func TestTransferWorkerIgnoresModelOverrideAndHonorsOperatorOption(t *testing.T) {
	dir := writeWorkerFixture(t)
	deployCalls := 0
	harness, err := NewHarness(Options{
		Dir: dir,
		Transfer: TransferCoordinator{Deploy: func(context.Context, deploy.EnvOpts, deploy.ProgressWriter) error {
			deployCalls++
			return nil
		}},
	})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	session, workspace, err := harness.Start(context.Background(), dir, "", "manual", "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	input := map[string]any{"env": "staging", "allow_failed": true}
	_, err = toolTransferWorker(context.Background(), ToolEnv{
		Harness: harness, Session: session, Workspace: &workspace,
	}, input)
	if err == nil || !strings.Contains(err.Error(), "transfer requires passing audit and review") {
		t.Fatalf("model override transfer error = %v, want audit/review gate", err)
	}
	if deployCalls != 0 {
		t.Fatalf("deploy calls after model override = %d, want 0", deployCalls)
	}

	result, err := toolTransferWorker(context.Background(), ToolEnv{
		Harness: harness, Session: session, Workspace: &workspace,
		Options: RuntimeOptions{AllowFail: true},
	}, map[string]any{"env": "staging"})
	if err != nil {
		t.Fatalf("operator override transfer error = %v", err)
	}
	if deployCalls != 1 {
		t.Fatalf("deploy calls after operator override = %d, want 1", deployCalls)
	}
	if result.Data["done"] != true {
		t.Fatalf("transfer result = %+v, want done", result)
	}
}

func TestWorkerFileToolsRejectSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeSymlink(t, filepath.Join(external, "secret.txt"), filepath.Join(root, "secret.txt"))
	makeSymlink(t, external, filepath.Join(root, "escape"))
	env := ToolEnv{Workspace: &Workspace{Dir: root}}

	if _, err := toolReadWorkerFile(context.Background(), env, map[string]any{"path": "secret.txt"}); err == nil {
		t.Fatal("read_worker_file accepted a symlink outside the workspace")
	}
	if _, err := toolWriteWorkerFile(context.Background(), env, map[string]any{
		"path": "escape/new.go", "content": "package escaped\n",
	}); err == nil {
		t.Fatal("write_worker_file accepted a real parent outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(external, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("external file stat error = %v, want no file", err)
	}
}

func TestScaffoldWorkerRejectsFactoryRootEscapes(t *testing.T) {
	tests := []struct {
		name      string
		worker    string
		requested func(t *testing.T, root, external string) string
	}{
		{
			name:   "absolute outside",
			worker: "absolute-outside",
			requested: func(_ *testing.T, _ string, external string) string {
				return external
			},
		},
		{
			name:   "absolute inside",
			worker: "absolute-inside",
			requested: func(t *testing.T, root, _ string) string {
				dir := filepath.Join(root, "nested")
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name:   "parent traversal",
			worker: "traversal",
			requested: func(_ *testing.T, _, _ string) string {
				return filepath.Join("..", "outside")
			},
		},
		{
			name:   "external parent symlink",
			worker: "parent-link",
			requested: func(t *testing.T, root, external string) string {
				makeSymlink(t, external, filepath.Join(root, "escape"))
				return "escape"
			},
		},
		{
			name:   "external target symlink",
			worker: "target-link",
			requested: func(t *testing.T, root, external string) string {
				target := filepath.Join(external, "target")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				makeSymlink(t, target, filepath.Join(root, "target-link"))
				return ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := t.TempDir()
			root := filepath.Join(container, "factory")
			external := filepath.Join(container, "outside")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(external, 0o755); err != nil {
				t.Fatal(err)
			}
			env := scaffoldToolEnv(t, root)
			requested := tt.requested(t, root, external)

			_, err := toolScaffoldWorker(context.Background(), env, map[string]any{
				"name": tt.worker, "trigger": "POST /tickets", "dir": requested,
			})
			if err == nil || !strings.Contains(err.Error(), "unsafe scaffold directory") {
				t.Fatalf("scaffold_worker error = %v, want unsafe directory rejection", err)
			}
		})
	}
}

func TestScaffoldWorkerAllowsReadableProjectBelowRealFactoryRoot(t *testing.T) {
	container := t.TempDir()
	root := filepath.Join(container, "factory")
	realParent := filepath.Join(root, "teams")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	makeSymlink(t, realParent, filepath.Join(root, "team-link"))
	env := scaffoldToolEnv(t, root)

	result, err := toolScaffoldWorker(context.Background(), env, map[string]any{
		"name": "ticket-worker", "trigger": "POST /tickets", "dir": "team-link",
	})
	if err != nil {
		t.Fatalf("scaffold_worker error = %v", err)
	}
	projectDir, _ := result.Data["dir"].(string)
	wantDir := filepath.Join(realParent, "ticket-worker")
	if projectDir != wantDir {
		t.Fatalf("project dir = %q, want canonical in-root path %q", projectDir, wantDir)
	}
	for _, name := range []string{"main.go", "go.mod", "pip.yaml"} {
		if _, err := os.Stat(filepath.Join(projectDir, name)); err != nil {
			t.Fatalf("readable project file %s missing: %v", name, err)
		}
	}
}

func scaffoldToolEnv(t *testing.T, root string) ToolEnv {
	t.Helper()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(root, "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	return ToolEnv{Harness: &Harness{Store: store}, Session: session}
}
