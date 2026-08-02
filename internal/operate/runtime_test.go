package operate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentRuntimeCreatesWorkerFromNaturalPrompt(t *testing.T) {
	parent := t.TempDir()
	// The hardened Git subprocess intentionally ignores ambient Git discovery
	// overrides such as GIT_CEILING_DIRECTORIES. Establish a real nested
	// repository boundary so this test stays isolated even when TMPDIR itself
	// lives under the Ouvrier checkout. Generated workers are ignored because
	// this factory test validates filesystem artifacts, not candidate diffs.
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("*\n!.gitignore\n"), 0o644); err != nil {
		t.Fatalf("write factory .gitignore: %v", err)
	}
	gitInitAndCommit(t, parent)
	buildCalls := 0
	harness, err := NewHarness(Options{
		Dir:    parent,
		Driver: ManualDriver{},
		Audit:  passingAuditRunner(),
		Builder: BuildCoordinator{GoRun: func(_ context.Context, _ string, _ []string, args []string, _, _ io.Writer) error {
			buildCalls++
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-o" {
					return os.WriteFile(args[i+1], []byte("worker binary"), 0o755)
				}
			}
			return errors.New("test build command omitted -o")
		}},
	})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: parent, Driver: ManualDriver{}, DriverID: "manual",
		HeadlessPosture: PostureAutoSafe, Harness: harness,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: parent, DriverID: "manual"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if started.Workspace != nil {
		t.Fatalf("started workspace = %+v, want nil in factory dir", started.Workspace)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "Create a worker that receives POST /tickets and triages tickets")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if turn.Workspace == nil || turn.Workspace.Name != "ticket-triage" {
		t.Fatalf("turn workspace = %+v, want ticket-triage", turn.Workspace)
	}
	for _, path := range []string{"main.go", "pip.yaml", "ouvrier.worker.json"} {
		if _, err := os.Stat(filepath.Join(parent, "ticket-triage", path)); err != nil {
			t.Fatalf("generated worker missing %s: %v", path, err)
		}
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if !transcriptHasTool(entries, "scaffold_worker") || !transcriptHasTool(entries, "patch_worker") {
		t.Fatalf("transcript entries = %+v, want scaffold and patch tools", entries)
	}
	loaded, err := runtime.Store.Load(started.Session.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !strings.HasSuffix(loaded.Dir, filepath.Join("ticket-triage")) {
		t.Fatalf("session dir = %q, want generated worker", loaded.Dir)
	}
	if buildCalls != 1 {
		t.Fatalf("build calls = %d, want one evidence-producing build", buildCalls)
	}

	readTurn, err := runtime.Prompt(context.Background(), started.Session.ID, "/read main.go")
	if err != nil {
		t.Fatalf("Prompt(/read) error = %v", err)
	}
	if !strings.Contains(readTurn.Final, "read main.go") {
		t.Fatalf("read final = %q, want file summary", readTurn.Final)
	}
	exportTurn, err := runtime.Prompt(context.Background(), started.Session.ID, "/export")
	if err != nil {
		t.Fatalf("Prompt(/export) error = %v", err)
	}
	if !strings.Contains(exportTurn.Final, "exported transcript") {
		t.Fatalf("export final = %q, want export summary", exportTurn.Final)
	}
	if _, err := os.Stat(loaded.ExportPath); err != nil {
		t.Fatalf("export artifact missing: %v", err)
	}
}

func TestHeadlessRuntimeFailsClosedOnWorkerMutationByDefault(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: parent, Driver: ManualDriver{}, DriverID: "manual"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: parent})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	_, err = runtime.Prompt(context.Background(), started.Session.ID, "Create a worker that receives POST /tickets")
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("Prompt() error = %v, want fail-closed ErrToolDenied", err)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "ticket-triage")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("headless mutation created worker despite manual posture: %v", statErr)
	}
}

func TestAgentRuntimeReviewUsesTranscriptAndReviewArtifact(t *testing.T) {
	dir := writeWorkerFixture(t)
	driver := &fakeDriver{
		result: TurnResult{FinalMessage: `{"passed":true,"summary":"ok","findings":[]}`},
	}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: driver, DriverID: "fake",
		HeadlessPosture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir, DriverID: "fake"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "/review governance")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if !strings.Contains(turn.Final, "review completed") {
		t.Fatalf("turn final = %q, want review summary", turn.Final)
	}
	if _, err := os.Stat(started.Session.ReviewPath); err != nil {
		t.Fatalf("review artifact missing: %v", err)
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if !transcriptHasTool(entries, "read_ouvrier_api") || !transcriptHasTool(entries, "review_worker") {
		t.Fatalf("transcript entries = %+v, want api and review tools", entries)
	}
}

func transcriptHasTool(entries []TranscriptEntry, name string) bool {
	for _, entry := range entries {
		if entry.Kind == TranscriptToolCall && entry.ToolName == name {
			return true
		}
	}
	return false
}
