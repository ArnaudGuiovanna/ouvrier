package operate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptJSONLBoundAcceptsAdvertisedWorkerWriteAndRejectsOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	store := NewTranscriptStore(path, Redactor{})
	// A one-MiB source body can expand to roughly six MiB under JSON escaping.
	// It must remain replayable because write_worker_file advertises that bound.
	content := strings.Repeat("\x01", 1<<20)
	if _, err := store.Append(TranscriptEntry{
		Kind: TranscriptToolCall, ToolName: "write_worker_file",
		Input:    map[string]any{"path": "generated.go", "content": content},
		Metadata: map[string]any{"tool_call_id": "bounded-write"},
	}); err != nil {
		t.Fatalf("Append(one-MiB escaped input) error = %v", err)
	}
	entries, err := ReadTranscript(path)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Input["content"] != content {
		t.Fatalf("replayed bounded transcript entry = %d entries / content preserved=%v", len(entries), len(entries) == 1 && entries[0].Input["content"] == content)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(TranscriptEntry{Kind: TranscriptAssistant, Text: strings.Repeat("x", maxJSONLLineBytes+1)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Append(oversize) error = %v, want bounded rejection", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("oversize append changed journal size from %d to %d", before.Size(), after.Size())
	}
}

func TestGenericJSONLWriterRejectsOversizeBeforeCreatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "tool-calls.jsonl")
	err := appendJSONLine(path, map[string]any{"payload": strings.Repeat("x", maxJSONLLineBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("appendJSONLine(oversize) error = %v, want bounded rejection", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("oversize JSONL record created %s: %v", path, statErr)
	}
}

func TestTranscriptRejectsOversizedFileBeforeAllocationOrAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxTranscriptFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTranscript(path); err == nil || !strings.Contains(err.Error(), "transcript exceeds") {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	store := NewTranscriptStore(path, Redactor{})
	if _, err := store.Append(TranscriptEntry{Kind: TranscriptStatus, Text: "must not append"}); err == nil || !strings.Contains(err.Error(), "transcript exceeds") {
		t.Fatalf("Append() error = %v", err)
	}
}
