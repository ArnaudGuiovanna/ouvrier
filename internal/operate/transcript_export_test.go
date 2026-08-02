package operate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportTranscriptMarkdownStreamsAtomicArtifact(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	store := NewTranscriptStore(transcript, Redactor{})
	if _, err := store.Append(TranscriptEntry{Kind: TranscriptUser, Text: "build a worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(TranscriptEntry{Kind: TranscriptToolResult, ToolName: "audit_worker", Output: map[string]any{"summary": "passed"}}); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "export.md")
	if err := exportTranscriptMarkdown(context.Background(), &Session{ID: "session", TranscriptPath: transcript}, destination); err != nil {
		t.Fatalf("exportTranscriptMarkdown() error = %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "## User") || !strings.Contains(string(data), "## Tool Result: audit_worker") {
		t.Fatalf("export = %q", data)
	}
}

func TestExportTranscriptRejectsSymlinkSourceAndCleansTemporaryOutput(t *testing.T) {
	dir := t.TempDir()
	external := filepath.Join(dir, "external.jsonl")
	if err := os.WriteFile(external, []byte(`{"kind":"user","text":"must not export"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, "transcript.jsonl")
	if err := os.Symlink(external, transcript); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "export.md")
	err := exportTranscriptMarkdown(context.Background(), &Session{ID: "session", TranscriptPath: transcript}, destination)
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("exportTranscriptMarkdown() error = %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("failed export published destination: %v", statErr)
	}
}

func TestExportTranscriptHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript.jsonl")
	store := NewTranscriptStore(transcript, Redactor{})
	if _, err := store.Append(TranscriptEntry{Kind: TranscriptStatus, Text: "ready"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	destination := filepath.Join(dir, "export.md")
	if err := exportTranscriptMarkdown(ctx, &Session{ID: "session", TranscriptPath: transcript}, destination); err == nil {
		t.Fatal("cancelled export returned nil error")
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled export published destination: %v", statErr)
	}
}
