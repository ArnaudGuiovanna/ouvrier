package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeMarksInterruptedToolCallOnce(t *testing.T) {
	dir := t.TempDir()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	call := TranscriptEntry{
		SessionID: started.Session.ID,
		Kind:      TranscriptToolCall,
		ToolName:  "side_effect",
		Input:     map[string]any{"secret": "kept"},
		Metadata:  map[string]any{"tool_call_id": "call-1"},
	}
	if _, err := runtime.transcript(started.Session).Append(call); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	resumer, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := resumer.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertInterruptedResultCount(t, resumed.Transcript, "call-1", 1)
	if err := resumer.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	resumedAgain, err := again.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertInterruptedResultCount(t, resumedAgain.Transcript, "call-1", 1)

	data, err := os.ReadFile(started.Session.ToolCallsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("tool audit lines = %d, want 1", len(lines))
	}
	var audit map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &audit); err != nil {
		t.Fatal(err)
	}
	if audit["tool_call_id"] != "call-1" {
		t.Fatalf("audit tool_call_id = %v", audit["tool_call_id"])
	}
}

func TestResumeAfterRealProcessKill(t *testing.T) {
	dir := t.TempDir()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	ready := filepath.Join(dir, "child-ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryKillHelper$")
	cmd.Env = append(os.Environ(),
		"OUVRIER_RECOVERY_HELPER=1",
		"OUVRIER_RECOVERY_DIR="+dir,
		"OUVRIER_RECOVERY_SESSION="+started.Session.ID,
		"OUVRIER_RECOVERY_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("helper did not durably persist the tool call")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	resumer, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumer.Close() })
	resumed, err := resumer.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertInterruptedResultCount(t, resumed.Transcript, "killed-call", 1)
}

func TestRecoveryKillHelper(t *testing.T) {
	if os.Getenv("OUVRIER_RECOVERY_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	dir := os.Getenv("OUVRIER_RECOVERY_DIR")
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Resume(context.Background(), os.Getenv("OUVRIER_RECOVERY_SESSION"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.transcript(started.Session).Append(TranscriptEntry{
		SessionID: started.Session.ID,
		Kind:      TranscriptToolCall,
		ToolName:  "killed-side-effect",
		Metadata:  map[string]any{"tool_call_id": "killed-call"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("OUVRIER_RECOVERY_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestResumeDoesNotAlterCompletedToolCall(t *testing.T) {
	dir := t.TempDir()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	store := runtime.transcript(started.Session)
	for _, entry := range []TranscriptEntry{
		{SessionID: started.Session.ID, Kind: TranscriptToolCall, ToolName: "done", Metadata: map[string]any{"tool_call_id": "call-2"}},
		{SessionID: started.Session.ID, Kind: TranscriptToolResult, ToolName: "done", Metadata: map[string]any{"tool_call_id": "call-2"}},
	} {
		if _, err := store.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	resumer, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumer.Close() })
	resumed, err := resumer.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for _, entry := range resumed.Transcript {
		if entry.Kind == TranscriptToolResult && metaString(entry.Metadata, "tool_call_id") == "call-2" {
			results++
			if entry.Metadata["recovered"] == true {
				t.Fatal("completed call was incorrectly recovered")
			}
		}
	}
	if results != 1 {
		t.Fatalf("completed results = %d, want 1", results)
	}
}

func TestSessionRejectsConcurrentWriter(t *testing.T) {
	dir := t.TempDir()
	first, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := first.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Resume(context.Background(), started.Session.ID); !errors.Is(err, ErrSessionWriterActive) {
		t.Fatalf("concurrent Resume() error = %v, want ErrSessionWriterActive", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := second.Resume(context.Background(), started.Session.ID); err != nil {
		t.Fatalf("Resume() after release error = %v", err)
	}
}

func TestReadTranscriptReportsTrailingAndMiddleCorruption(t *testing.T) {
	valid := `{"id":"1","session_id":"s","kind":"status"}` + "\n"
	for _, tc := range []struct {
		name    string
		content string
		line    string
	}{
		{name: "trailing", content: valid + `{"id":`, line: "line 2"},
		{name: "middle", content: valid + "not-json\n" + valid, line: "line 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/transcript.jsonl"
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadTranscript(path)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.line) {
				t.Fatalf("ReadTranscript() error = %v, want path and %s", err, tc.line)
			}
		})
	}
}

func assertInterruptedResultCount(t *testing.T, entries []TranscriptEntry, id string, want int) {
	t.Helper()
	var count int
	for _, entry := range entries {
		if entry.Kind != TranscriptToolResult || metaString(entry.Metadata, "tool_call_id") != id {
			continue
		}
		count++
		if entry.Metadata["recovered"] != true || entry.Output["interrupted"] != true {
			t.Fatalf("recovered result = %+v", entry)
		}
	}
	if count != want {
		t.Fatalf("interrupted results for %s = %d, want %d", id, count, want)
	}
}
