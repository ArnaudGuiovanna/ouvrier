package operate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

type fakeDriver struct {
	req    TurnRequest
	result TurnResult
	run    func(TurnRequest) error
}

func (d *fakeDriver) Probe(context.Context) (Capabilities, error) {
	return Capabilities{Name: "fake", Transport: "test", Authenticated: true}, nil
}

func (d *fakeDriver) RunTurn(_ context.Context, req TurnRequest, _ EventSink) (TurnResult, error) {
	d.req = req
	if d.run != nil {
		if err := d.run(req); err != nil {
			return TurnResult{}, err
		}
	}
	return d.result, nil
}

func (d *fakeDriver) Close() error { return nil }

func TestStorePersistsSessionsAndArtifacts(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	session, err := store.Create(dir, "codex", "exec")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if session.Status != StatusNew || session.PatchPath == "" {
		t.Fatalf("session = %+v, want new status with patch path", session)
	}

	if err := store.Transition(session, StatusSelected, "selected"); err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	path, err := store.WriteArtifact(session, "note.json", []byte("{}\n"))
	if err != nil {
		t.Fatalf("WriteArtifact() error = %v", err)
	}
	if !strings.HasPrefix(path, store.SessionDir(session.ID)) {
		t.Fatalf("artifact path = %q, want under session dir", path)
	}

	loaded, err := store.Load(session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Status != StatusSelected || len(loaded.Transitions) != 2 {
		t.Fatalf("loaded session = %+v", loaded)
	}
}

func TestReviewWorkerUsesReadOnlyDriverAndParsesFindings(t *testing.T) {
	dir := writeWorkerFixture(t)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatalf("DetectWorkspace() error = %v", err)
	}
	driver := &fakeDriver{
		result: TurnResult{FinalMessage: `{"summary":"needs governance","findings":[{"severity":"high","file":"main.go","line":12,"title":"missing policy","body":"tool needs SideEffecting"}]}`},
	}

	report, err := ReviewWorker(context.Background(), driver, ReviewRequest{Workspace: ws, Scope: ReviewGovernance}, nil)
	if err != nil {
		t.Fatalf("ReviewWorker() error = %v", err)
	}
	if driver.req.Kind != TurnReview || driver.req.Sandbox != SandboxReadOnly {
		t.Fatalf("driver request = %+v, want read-only review", driver.req)
	}
	if report.Summary != "needs governance" || len(report.Findings) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if !ReviewHasBlockingFindings(report) {
		t.Fatal("ReviewHasBlockingFindings() = false, want true")
	}
}

func TestPatchWorkerRequiresCleanGitAndCapturesDiff(t *testing.T) {
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatalf("DetectWorkspace() error = %v", err)
	}
	driver := &fakeDriver{
		result: TurnResult{FinalMessage: `{"summary":"updated handler","changed_files":["main.go"]}`},
		run: func(req TurnRequest) error {
			if req.Sandbox != SandboxWorkspaceWrite {
				t.Fatalf("sandbox = %q, want workspace-write", req.Sandbox)
			}
			path := filepath.Join(req.CWD, "main.go")
			return os.WriteFile(path, []byte("package main\n\nfunc main() { println(\"updated\") }\n"), 0o644)
		},
	}

	report, err := PatchWorker(context.Background(), driver, PatchRequest{Workspace: ws, Goal: "make the worker visible"}, nil)
	if err != nil {
		t.Fatalf("PatchWorker() error = %v", err)
	}
	if driver.req.Kind != TurnPatch {
		t.Fatalf("turn kind = %q, want patch", driver.req.Kind)
	}
	if report.Summary != "updated handler" {
		t.Fatalf("summary = %q", report.Summary)
	}
	if !strings.Contains(report.Diff.Diff, "updated") {
		t.Fatalf("diff = %q, want updated source", report.Diff.Diff)
	}
	if len(report.ChangedFiles) != 1 || report.ChangedFiles[0] != "main.go" {
		t.Fatalf("changed files = %v", report.ChangedFiles)
	}
}

func TestDetectWorkspaceIgnoresOperateSessionStateInGitStatus(t *testing.T) {
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".ouvrier", "operate", "sessions", "s"), 0o700); err != nil {
		t.Fatalf("mkdir .ouvrier: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ouvrier", "operate", "sessions", "s", "session.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	ws, err := DetectWorkspace(dir)
	if err != nil {
		t.Fatalf("DetectWorkspace() error = %v", err)
	}
	if ws.Git.Dirty {
		t.Fatalf("git status = %q, want clean after ignoring .ouvrier", ws.Git.Status)
	}
}

func TestAuditRunnerRunsInjectedGatesAndSecretScan(t *testing.T) {
	dir := writeWorkerFixture(t)
	runner := AuditRunner{
		RunCommand: func(_ context.Context, _ string, name string, args []string) (string, string, error) {
			if name == "git" && len(args) >= 2 && args[0] == "diff" {
				return "+OPENAI_API_KEY=sk-test\n", "", nil
			}
			return "", "", nil
		},
		Build: func(context.Context, string, *bytes.Buffer, *bytes.Buffer) error {
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 6, 13, 11, 0, 0, 0, time.UTC) },
	}

	report, err := runner.Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false for secret-shaped diff")
	}
	found := false
	for _, gate := range report.Results {
		if gate.Name == "secret scan" && gate.Status == GateFail {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret scan failure missing from %+v", report.Results)
	}
}

func TestTransferRequiresAuditAndReviewBeforeDeploy(t *testing.T) {
	called := false
	coordinator := TransferCoordinator{
		Deploy: func(context.Context, deploy.EnvOpts, deploy.ProgressWriter) error {
			called = true
			return nil
		},
	}

	_, err := coordinator.Transfer(context.Background(), TransferRequest{Dir: t.TempDir(), Env: "staging"}, ProgressWriter{})
	if err == nil {
		t.Fatal("Transfer() error = nil, want gate error")
	}
	if called {
		t.Fatal("Deploy called despite failed gates")
	}
}

func writeWorkerFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("pip.yaml", `name: demo
version: 0.1.0
deploy:
  staging:
    hosts: [deploy@example.com]
`)
	write("main.go", "package main\n\nfunc main() {}\n")
	write("ouvrier.worker.json", `{"name":"demo","events":["POST /tickets"],"outcomes":["triage"],"admin_url":"http://127.0.0.1:8080"}`+"\n")
	return dir
}

func gitInitAndCommit(t *testing.T, dir string) {
	t.Helper()
	commands := [][]string{
		{"git", "init"},
		{"git", "add", "."},
		{"git", "-c", "user.name=Ouvrier Test", "-c", "user.email=test@example.com", "commit", "-m", "initial"},
	}
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}
