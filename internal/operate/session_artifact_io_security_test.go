package operate

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionJournalWritersRejectFinalSymlink(t *testing.T) {
	tests := []struct {
		name  string
		write func(string) error
	}{
		{
			name: "generic JSONL append",
			write: func(path string) error {
				return appendJSONLine(path, map[string]any{"kind": "tool_call"})
			},
		},
		{
			name: "transcript",
			write: func(path string) error {
				_, err := NewTranscriptStore(path, Redactor{}).Append(TranscriptEntry{
					Kind: TranscriptStatus,
					Text: "checkpoint",
				})
				return err
			},
		},
		{
			name: "events",
			write: func(path string) error {
				return NewEventLog(path, Redactor{}).Event(Event{Kind: EventFinal, Message: "done"})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			external := filepath.Join(dir, "protected")
			const protected = "must remain unchanged\n"
			if err := os.WriteFile(external, []byte(protected), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "journal.jsonl")
			if err := os.Symlink(external, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			if err := tc.write(path); err == nil {
				t.Fatal("journal writer followed a final symlink")
			}
			data, err := os.ReadFile(external)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != protected {
				t.Fatalf("external file changed through final symlink: %q", data)
			}
		})
	}
}

func TestSessionJournalAppendRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	parent := filepath.Join(root, "session")
	if err := os.Symlink(external, parent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(parent, "tool-calls.jsonl")

	if err := appendJSONLine(path, map[string]any{"tool": "must-not-escape"}); err == nil {
		t.Fatal("appendJSONLine followed a symlinked parent")
	}
	if _, err := os.Lstat(filepath.Join(external, "tool-calls.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("journal escaped into external directory: %v", err)
	}
}

func TestSessionJournalReadersAndRepairRejectFinalSymlink(t *testing.T) {
	tests := []struct {
		name string
		body string
		read func(string) error
	}{
		{
			name: "transcript",
			body: `{"kind":"status","text":"external"}` + "\n",
			read: func(path string) error {
				_, err := ReadTranscript(path)
				return err
			},
		},
		{
			name: "events",
			body: `{"kind":"final","message":"external"}` + "\n",
			read: func(path string) error {
				_, err := readEvents(path)
				return err
			},
		},
		{
			name: "tool-call audit",
			body: `{"tool_call_id":"external"}` + "\n",
			read: func(path string) error {
				_, err := readToolCallAuditIDs(path)
				return err
			},
		},
		{
			name: "tail repair",
			body: `{"tool_call_id":"external"}`,
			read: func(path string) error {
				_, err := repairJSONLTail(path, "test journal")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			external := filepath.Join(dir, "external.jsonl")
			if err := os.WriteFile(external, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "artifact.jsonl")
			if err := os.Symlink(external, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			if err := tc.read(path); err == nil {
				t.Fatal("artifact reader followed a final symlink")
			}
			data, err := os.ReadFile(external)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.body {
				t.Fatalf("external artifact changed during read/repair: %q", data)
			}
		})
	}
}

func TestSessionAtomicArtifactRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	parent := filepath.Join(root, "session")
	if err := os.Symlink(external, parent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(parent, "session.json")

	if err := writeAtomic(path, []byte("must-not-escape\n"), 0o600); err == nil {
		t.Fatal("writeAtomic followed a symlinked parent")
	}
	if _, err := os.Lstat(filepath.Join(external, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("atomic artifact escaped into external directory: %v", err)
	}
}

func TestSessionAtomicArtifactFailsClosedOnParentExchange(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	parent := filepath.Join(root, "session")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "session-original")
	path := filepath.Join(parent, "session.json")
	exchanged := false

	err := writeAtomicStream(path, 0o600, func(writer io.Writer) error {
		if err := os.Rename(parent, original); err != nil {
			return err
		}
		if err := os.Symlink(external, parent); err != nil {
			return err
		}
		exchanged = true
		_, err := writer.Write([]byte("must-not-publish\n"))
		return err
	})
	if !exchanged {
		t.Fatal("test did not exchange the artifact parent")
	}
	if err == nil {
		t.Fatal("writeAtomicStream accepted an exchanged artifact parent")
	}
	for _, candidate := range []string{
		filepath.Join(external, "session.json"),
		filepath.Join(original, "session.json"),
	} {
		if _, statErr := os.Lstat(candidate); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed atomic write published %s: %v", candidate, statErr)
		}
	}
}

func TestTranscriptExportRejectsSymlinkedSourceParent(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	externalTranscript := filepath.Join(external, "transcript.jsonl")
	if err := os.WriteFile(externalTranscript, []byte(`{"kind":"user","text":"external"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "session")
	if err := os.Symlink(external, parent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	destination := filepath.Join(root, "export.md")
	err := exportTranscriptMarkdown(context.Background(), &Session{
		ID:             "session",
		TranscriptPath: filepath.Join(parent, "transcript.jsonl"),
	}, destination)
	if err == nil {
		t.Fatal("transcript export followed a symlinked source parent")
	}
	if _, statErr := os.Lstat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("rejected export published an artifact: %v", statErr)
	}
}

func TestStoreLoadRejectsFinalSessionStateSymlink(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(store.SessionDir(session.ID), "session.json")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-session.json")
	if err := os.WriteFile(external, state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, statePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := store.Load(session.ID); err == nil {
		t.Fatal("Store.Load followed a final session-state symlink")
	}
}

func TestSessionWriterLockRejectsFinalSymlink(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-lock")
	if err := os.WriteFile(external, []byte("protected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(store.SessionDir(session.ID), "writer.lock")
	if err := os.Symlink(external, lockPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runtime := &AgentRuntime{Store: store, locks: make(map[string]*os.File)}

	if _, err := runtime.lockSession(session); err == nil {
		t.Fatal("session writer lock followed a final symlink")
	}
}

func TestSubscribePropagatesUnsafeEventJournal(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-events.jsonl")
	if err := os.WriteFile(external, []byte(`{"kind":"final","message":"external"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, session.EventsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runtime := &AgentRuntime{Store: store}

	if _, err := runtime.Subscribe(context.Background(), session.ID); err == nil {
		t.Fatal("Subscribe hid an unsafe event journal")
	}
}
