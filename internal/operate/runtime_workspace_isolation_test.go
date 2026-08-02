package operate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentRuntimeConcurrentSessionsKeepWorkspaceIsolated(t *testing.T) {
	root := t.TempDir()
	alphaDir := writeNamedWorkerAt(t, root, "alpha")
	betaDir := writeNamedWorkerAt(t, root, "beta")

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	registry := NewToolRegistry()
	registry.Register(Tool{
		Name:       "read_ouvrier_api",
		Governance: GovReadOnly,
		Run: func(ctx context.Context, _ ToolEnv, _ map[string]any) (ToolResult, error) {
			ready <- struct{}{}
			select {
			case <-ctx.Done():
				return ToolResult{}, ctx.Err()
			case <-release:
				return ToolResult{Summary: "loaded API context"}, nil
			}
		},
	})
	registry.Register(Tool{
		Name:       "review_worker",
		Governance: GovReadOnly,
		Run: func(_ context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
			workspace, err := requireWorkspace(env)
			if err != nil {
				return ToolResult{}, err
			}
			return ToolResult{Summary: fmt.Sprintf("reviewed %s at %s", workspace.Name, workspace.Dir)}, nil
		},
	})

	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: root, Driver: ManualDriver{}, Tools: registry})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	alpha, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: alphaDir})
	if err != nil {
		t.Fatalf("Start(alpha) error = %v", err)
	}
	beta, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: betaDir})
	if err != nil {
		t.Fatalf("Start(beta) error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type turnResult struct {
		name string
		turn RuntimeTurn
		err  error
	}
	results := make(chan turnResult, 2)
	go func() {
		turn, runErr := runtime.Prompt(ctx, alpha.Session.ID, "review this worker")
		results <- turnResult{name: "alpha", turn: turn, err: runErr}
	}()
	go func() {
		turn, runErr := runtime.Prompt(ctx, beta.Session.ID, "review this worker")
		results <- turnResult{name: "beta", turn: turn, err: runErr}
	}()

	for calls := 0; calls < 2; calls++ {
		select {
		case <-ready:
		case result := <-results:
			close(release)
			t.Fatalf("Prompt(%s) returned before synchronization: %v", result.name, result.err)
		case <-ctx.Done():
			close(release)
			t.Fatalf("concurrent prompts did not reach tool barrier: %v", ctx.Err())
		}
	}
	close(release)

	for completed := 0; completed < 2; completed++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("Prompt(%s) error = %v", result.name, result.err)
			}
			wantDir := alphaDir
			if result.name == "beta" {
				wantDir = betaDir
			}
			if !strings.Contains(result.turn.Final, "reviewed "+result.name+" at "+wantDir) {
				t.Fatalf("Prompt(%s) final = %q, want its own workspace %q", result.name, result.turn.Final, wantDir)
			}
			if result.turn.Workspace == nil || result.turn.Workspace.Name != result.name || result.turn.Workspace.Dir != wantDir {
				t.Fatalf("Prompt(%s) workspace = %+v, want %s at %s", result.name, result.turn.Workspace, result.name, wantDir)
			}
		case <-ctx.Done():
			t.Fatalf("concurrent prompts did not complete: %v", ctx.Err())
		}
	}
}

func TestAgentRuntimeScaffoldUpdatesOnlyCallingSessionWorkspace(t *testing.T) {
	root := t.TempDir()
	registry := NewToolRegistry()
	registry.Register(Tool{
		Name:       "probe_workspace",
		Governance: GovReadOnly,
		Run: func(_ context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
			if env.Workspace == nil {
				return ToolResult{Summary: "no workspace"}, nil
			}
			return ToolResult{Summary: env.Workspace.Name, Data: map[string]any{"dir": env.Workspace.Dir}}, nil
		},
	})
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: root, Driver: ManualDriver{}, Tools: registry})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	first, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: root})
	if err != nil {
		t.Fatalf("Start(first) error = %v", err)
	}
	second, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: root})
	if err != nil {
		t.Fatalf("Start(second) error = %v", err)
	}

	_, err = runtime.Executor().Execute(context.Background(), GovernedCall{
		Session: first.Session,
		Tool:    "scaffold_worker",
		Input: map[string]any{
			"name": "first-worker", "trigger": "POST /tickets", "model": defaultOperateModel,
		},
		Posture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("scaffold_worker error = %v", err)
	}

	firstProbe, err := runtime.Executor().Execute(context.Background(), GovernedCall{Session: first.Session, Tool: "probe_workspace"})
	if err != nil {
		t.Fatalf("probe first session error = %v", err)
	}
	secondProbe, err := runtime.Executor().Execute(context.Background(), GovernedCall{Session: second.Session, Tool: "probe_workspace"})
	if err != nil {
		t.Fatalf("probe second session error = %v", err)
	}
	if firstProbe.Summary != "first-worker" {
		t.Fatalf("first session workspace = %q, want first-worker", firstProbe.Summary)
	}
	if secondProbe.Summary != "no workspace" {
		t.Fatalf("second session workspace = %q, want no workspace", secondProbe.Summary)
	}

	secondTurn, err := runtime.Prompt(context.Background(), second.Session.ID, "improve the selected worker")
	if err != nil {
		t.Fatalf("Prompt(second) error = %v", err)
	}
	if !strings.Contains(secondTurn.Final, "No worker is selected yet") {
		t.Fatalf("Prompt(second) final = %q, want no-worker guidance", secondTurn.Final)
	}
	entries, err := ReadTranscript(second.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript(second) error = %v", err)
	}
	if transcriptHasTool(entries, "patch_worker") {
		t.Fatal("factory session planned patch_worker using another session's workspace")
	}
}

func writeNamedWorkerAt(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir worker %s: %v", name, err)
	}
	files := map[string]string{
		"pip.yaml":            "name: " + name + "\nversion: 0.1.0\n",
		"main.go":             "package main\n\nfunc main() {}\n",
		"ouvrier.worker.json": fmt.Sprintf("{\"name\":%q,\"events\":[\"POST /tickets\"],\"outcomes\":[\"ok\"]}\n", name),
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", name, rel, err)
		}
	}
	return dir
}
