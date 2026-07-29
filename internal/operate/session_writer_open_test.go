package operate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestOpenSessionWriterRepairsTornTailOnlyAfterAcquiringLock(t *testing.T) {
	dir := t.TempDir()
	owner, session := startSessionWriterRuntime(t, dir)
	data, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	torn := append(append([]byte(nil), data...), []byte(`{"partial"`)...)
	if err := os.WriteFile(session.TranscriptPath, torn, 0o600); err != nil {
		t.Fatalf("write torn transcript: %v", err)
	}

	contender, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	_, err = contender.OpenSessionWriter(context.Background(), RuntimeStartRequest{SessionID: session.ID})
	if !errors.Is(err, ErrSessionWriterActive) {
		t.Fatalf("OpenSessionWriter() error = %v, want ErrSessionWriterActive", err)
	}
	unchanged, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.Equal(unchanged, torn) {
		t.Fatalf("conflicting writer repaired transcript before acquiring lock:\n got %q\nwant %q", unchanged, torn)
	}
	if err := contender.Close(); err != nil {
		t.Fatalf("contender Close() error = %v", err)
	}

	if err := owner.Close(); err != nil {
		t.Fatalf("owner Close() error = %v", err)
	}
	opened, err := contender.OpenSessionWriter(context.Background(), RuntimeStartRequest{SessionID: session.ID})
	if err != nil {
		t.Fatalf("OpenSessionWriter() after release error = %v", err)
	}
	if len(opened.Transcript) == 0 || !strings.Contains(opened.Transcript[len(opened.Transcript)-1].Text, "discarded") {
		t.Fatalf("opened transcript = %+v, want durable torn-tail recovery entry", opened.Transcript)
	}
	repaired, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if bytes.Contains(repaired, []byte(`{"partial"`)) {
		t.Fatalf("repaired transcript still contains torn tail: %q", repaired)
	}
	if _, err := ReadTranscript(session.TranscriptPath); err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if err := contender.Close(); err != nil {
		t.Fatalf("contender Close() error = %v", err)
	}
}

func TestOpenSessionWriterHoldsNewSessionLockUntilClose(t *testing.T) {
	dir := writeWorkerFixture(t)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.OpenSessionWriter(context.Background(), RuntimeStartRequest{
		Dir:           dir,
		InitialPrompt: "review the worker",
		DriverID:      "manual",
	})
	if err != nil {
		t.Fatalf("OpenSessionWriter() error = %v", err)
	}
	goal, err := os.ReadFile(started.Session.GoalPath)
	if err != nil {
		t.Fatalf("ReadFile(goal) error = %v", err)
	}
	if string(goal) != "review the worker\n" {
		t.Fatalf("goal = %q, want initial prompt", goal)
	}

	other, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	if _, err := other.OpenSessionWriter(context.Background(), RuntimeStartRequest{SessionID: started.Session.ID}); !errors.Is(err, ErrSessionWriterActive) {
		t.Fatalf("concurrent OpenSessionWriter() error = %v, want ErrSessionWriterActive", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := other.OpenSessionWriter(context.Background(), RuntimeStartRequest{SessionID: started.Session.ID}); err != nil {
		t.Fatalf("OpenSessionWriter() after Close error = %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("other Close() error = %v", err)
	}
}
