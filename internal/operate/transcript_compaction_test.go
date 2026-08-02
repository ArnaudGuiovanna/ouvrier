package operate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCompactPersistsBoundedContextBoundaryUsedByModelHistory(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.appendTranscript(started.Session, TranscriptEntry{Kind: TranscriptUser, Role: "user", Text: "inspect the worker carefully"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.appendTranscript(started.Session, TranscriptEntry{Kind: TranscriptAssistant, Role: "assistant", Text: "the worker was inspected"}); err != nil {
		t.Fatal(err)
	}

	turn, err := runtime.Compact(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if !strings.Contains(turn.Final, "context compacted") || len(turn.Entries) != 1 {
		t.Fatalf("Compact() turn = %+v", turn)
	}
	checkpoint := turn.Entries[0]
	if checkpoint.Metadata["context_compaction"] != true || metaString(checkpoint.Metadata, "context_summary_sha256") == "" {
		t.Fatalf("checkpoint metadata = %+v", checkpoint.Metadata)
	}
	summary := metaString(checkpoint.Metadata, "context_summary")
	if summary == "" || len(summary) > maxCompactionSummaryBytes {
		t.Fatalf("context summary bytes = %d", len(summary))
	}
	if _, err := runtime.appendTranscript(started.Session, TranscriptEntry{Kind: TranscriptUser, Role: "user", Text: "current request"}); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := historyMessages(entries)
	if err != nil {
		t.Fatalf("historyMessages() error = %v", err)
	}
	if len(messages) != 2 || !strings.Contains(messages[0].Text(), "durable context checkpoint") || messages[1].Text() != "current request" {
		t.Fatalf("compacted messages = %#v", messages)
	}
}

func TestCompactRefusesDanglingDurableToolCall(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.appendTranscript(started.Session, TranscriptEntry{
		Kind: TranscriptToolCall, ToolName: "list_workers", Metadata: map[string]any{"tool_call_id": "pending-compact"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Compact(context.Background(), started.Session.ID); err == nil || !strings.Contains(err.Error(), "lack results") {
		t.Fatalf("Compact() error = %v", err)
	}
}

func TestPromptSlashCompactUsesDurableCompactionPrimitive(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.appendTranscript(started.Session, TranscriptEntry{Kind: TranscriptUser, Role: "user", Text: "earlier request"}); err != nil {
		t.Fatal(err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "/compact")
	if err != nil {
		t.Fatalf("Prompt(/compact) error = %v", err)
	}
	if len(turn.Entries) != 1 || turn.Entries[0].Metadata["context_compaction"] != true {
		t.Fatalf("Prompt(/compact) turn = %+v", turn)
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == TranscriptUser && entry.Text == "/compact" {
			t.Fatalf("slash command was added to model context: %+v", entries)
		}
	}
}

func TestModelHistoryRequiresCompactionAtEntryBound(t *testing.T) {
	entries := make([]TranscriptEntry, maxModelHistoryEntries+1)
	for i := range entries {
		entries[i] = TranscriptEntry{Kind: TranscriptUser, Text: "bounded"}
	}
	_, err := historyMessages(entries)
	if !errors.Is(err, ErrContextCompactionRequired) {
		t.Fatalf("historyMessages() error = %v", err)
	}
}
