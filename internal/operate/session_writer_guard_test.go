package operate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type runtimeMutation struct {
	name string
	text string
	call func(*AgentRuntime, string, string) error
}

func sessionWriterMutations() []runtimeMutation {
	return []runtimeMutation{
		{
			name: "prompt",
			text: "blocked prompt",
			call: func(runtime *AgentRuntime, sessionID, text string) error {
				_, err := runtime.Prompt(context.Background(), sessionID, text)
				return err
			},
		},
		{
			name: "steer",
			text: "blocked steer",
			call: func(runtime *AgentRuntime, sessionID, text string) error {
				_, err := runtime.Steer(context.Background(), sessionID, text)
				return err
			},
		},
		{
			name: "follow_up",
			text: "blocked follow up",
			call: func(runtime *AgentRuntime, sessionID, text string) error {
				_, err := runtime.FollowUp(context.Background(), sessionID, text)
				return err
			},
		},
		{
			name: "interrupt",
			text: "blocked interrupt",
			call: func(runtime *AgentRuntime, sessionID, text string) error {
				_, err := runtime.Interrupt(context.Background(), sessionID, text)
				return err
			},
		},
		{
			name: "compact",
			call: func(runtime *AgentRuntime, sessionID, _ string) error {
				_, err := runtime.Compact(context.Background(), sessionID)
				return err
			},
		},
	}
}

func TestSessionMutationsRequireOwnedWriterLockWithoutWriting(t *testing.T) {
	for _, mutation := range sessionWriterMutations() {
		t.Run(mutation.name, func(t *testing.T) {
			dir := t.TempDir()
			owner, session := startSessionWriterRuntime(t, dir)
			if err := owner.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			before := snapshotSessionFiles(t, owner.Store.SessionDir(session.ID))

			unopened, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
			if err != nil {
				t.Fatalf("NewAgentRuntime() error = %v", err)
			}
			t.Cleanup(func() { _ = unopened.Close() })
			err = mutation.call(unopened, session.ID, mutation.text)
			if !errors.Is(err, ErrSessionWriterNotHeld) {
				t.Fatalf("%s error = %v, want ErrSessionWriterNotHeld", mutation.name, err)
			}

			after := snapshotSessionFiles(t, owner.Store.SessionDir(session.ID))
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed session files without owning the writer lock:\nbefore=%v\nafter=%v", mutation.name, before, after)
			}
		})
	}
}

func TestSessionMutationErrorsDistinguishWriterStates(t *testing.T) {
	dir := t.TempDir()
	unopened, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = unopened.Close() })

	if _, err := unopened.Prompt(context.Background(), "missing-session", "hello"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing Prompt() error = %v, want ErrSessionNotFound", err)
	}

	owner, session := startSessionWriterRuntime(t, dir)
	if _, err := unopened.Prompt(context.Background(), session.ID, "hello"); !errors.Is(err, ErrSessionWriterActive) {
		t.Fatalf("active-writer Prompt() error = %v, want ErrSessionWriterActive", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := unopened.Prompt(context.Background(), session.ID, "hello"); !errors.Is(err, ErrSessionWriterNotHeld) {
		t.Fatalf("unopened Prompt() error = %v, want ErrSessionWriterNotHeld", err)
	}
}

func TestRejectedPromptPreservesTornTailForResumeRepair(t *testing.T) {
	dir := t.TempDir()
	owner, session := startSessionWriterRuntime(t, dir)
	if err := owner.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	data, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	data = []byte(strings.TrimSuffix(string(data), "\n"))
	if err := os.WriteFile(session.TranscriptPath, data, 0o600); err != nil {
		t.Fatalf("write torn transcript: %v", err)
	}
	before := snapshotSessionFiles(t, owner.Store.SessionDir(session.ID))

	unopened, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	if _, err := unopened.Prompt(context.Background(), session.ID, "hello"); !errors.Is(err, ErrSessionWriterNotHeld) {
		t.Fatalf("Prompt() error = %v, want ErrSessionWriterNotHeld", err)
	}
	after := snapshotSessionFiles(t, owner.Store.SessionDir(session.ID))
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected Prompt changed torn session files:\nbefore=%v\nafter=%v", before, after)
	}

	resumer, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = resumer.Close() })
	if _, err := resumer.Resume(context.Background(), session.ID); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	repaired, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.HasSuffix(string(repaired), "\n") {
		t.Fatalf("repaired transcript does not end in a newline: %q", repaired)
	}
	if _, err := ReadTranscript(session.TranscriptPath); err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
}

func TestConcurrentRuntimeMutationsCannotInterleaveSessionWrites(t *testing.T) {
	dir := t.TempDir()
	owner, session := startSessionWriterRuntime(t, dir)
	t.Cleanup(func() { _ = owner.Close() })
	other, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	ownerErr := make(chan error, 1)
	otherErrs := make(chan error, len(sessionWriterMutations()))
	go func() {
		defer wg.Done()
		<-start
		_, err := owner.Prompt(context.Background(), session.ID, "/tools")
		ownerErr <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		for _, mutation := range sessionWriterMutations() {
			otherErrs <- mutation.call(other, session.ID, mutation.text)
		}
	}()
	close(start)
	wg.Wait()
	close(ownerErr)
	close(otherErrs)

	if err := <-ownerErr; err != nil {
		t.Fatalf("owner Prompt() error = %v", err)
	}
	for err := range otherErrs {
		if !errors.Is(err, ErrSessionWriterActive) {
			t.Fatalf("other mutation error = %v, want ErrSessionWriterActive", err)
		}
	}
	entries, err := ReadTranscript(session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	var transcriptText strings.Builder
	for _, entry := range entries {
		transcriptText.WriteString(entry.Text)
		transcriptText.WriteByte('\n')
	}
	for _, mutation := range sessionWriterMutations() {
		if mutation.text != "" && strings.Contains(transcriptText.String(), mutation.text) {
			t.Fatalf("transcript contains rejected %s write %q", mutation.name, mutation.text)
		}
	}
	if !strings.Contains(transcriptText.String(), "/tools") {
		t.Fatalf("transcript does not contain the owner write: %q", transcriptText.String())
	}
}

func TestGovernedExecutorRequiresOwnedWriterLockBeforeSideEffect(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	owner, session := startSessionWriterRuntime(t, dir)
	if err := owner.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	before := snapshotSessionFiles(t, owner.Store.SessionDir(session.ID))

	unopened, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = unopened.Close() })
	_, err = unopened.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "write_worker_file",
		Input:   map[string]any{"path": "must-not-exist.txt", "content": "blocked"},
		Posture: PostureAutoSafe,
	})
	if !errors.Is(err, ErrSessionWriterNotHeld) {
		t.Fatalf("Execute() error = %v, want ErrSessionWriterNotHeld", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "must-not-exist.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("governed side effect was not blocked: %v", err)
	}
	after := snapshotSessionFiles(t, owner.Store.SessionDir(session.ID))
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected governed call changed session files:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestForkLeavesParentUntouchedAndOwnsChildWriterLock(t *testing.T) {
	dir := t.TempDir()
	runtime, parent := startSessionWriterRuntime(t, dir)
	t.Cleanup(func() { _ = runtime.Close() })
	beforeParent := snapshotSessionFiles(t, runtime.Store.SessionDir(parent.ID))

	forked, err := runtime.Fork(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Fork() error = %v", err)
	}
	afterParent := snapshotSessionFiles(t, runtime.Store.SessionDir(parent.ID))
	if !reflect.DeepEqual(afterParent, beforeParent) {
		t.Fatalf("Fork changed parent session files:\nbefore=%v\nafter=%v", beforeParent, afterParent)
	}
	if err := runtime.requireSessionWriter(forked.Session); err != nil {
		t.Fatalf("forked session writer ownership error = %v", err)
	}

	other, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, err := other.Prompt(context.Background(), forked.Session.ID, "blocked child prompt"); !errors.Is(err, ErrSessionWriterActive) {
		t.Fatalf("child Prompt() error = %v, want ErrSessionWriterActive", err)
	}
	entries, err := ReadTranscript(forked.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Metadata["parent_session"] != parent.ID {
		t.Fatalf("forked transcript = %+v, want one parent reference", entries)
	}
}

func startSessionWriterRuntime(t *testing.T, dir string) (*AgentRuntime, *Session) {
	t.Helper()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return runtime, started.Session
}

func snapshotSessionFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot session files: %v", err)
	}
	return snapshot
}
