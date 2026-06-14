package operate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentRuntimeCreatesWorkerFromNaturalPrompt(t *testing.T) {
	parent := t.TempDir()
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir:      parent,
		Driver:   ManualDriver{},
		DriverID: "manual",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
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

func TestAgentRuntimeReviewUsesTranscriptAndReviewArtifact(t *testing.T) {
	dir := writeWorkerFixture(t)
	driver := &fakeDriver{
		result: TurnResult{FinalMessage: `{"summary":"ok","findings":[]}`},
	}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir:      dir,
		Driver:   driver,
		DriverID: "fake",
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
