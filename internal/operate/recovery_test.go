package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
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

func TestResumePreservesOneAssistantGroupForMultipleToolCalls(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	toolStarted := make(chan struct{})
	executions := make(chan string, 2)
	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{
		Name:       "blocking_probe",
		Governance: GovReadOnly,
		Run: func(ctx context.Context, _ ToolEnv, input map[string]any) (ToolResult, error) {
			id, _ := input["id"].(string)
			executions <- id
			if id == "first" {
				close(toolStarted)
				<-ctx.Done()
				return ToolResult{Summary: "first interrupted"}, ctx.Err()
			}
			return ToolResult{Summary: "second must only be recovered"}, nil
		},
	})
	toolSchemas["blocking_probe"] = `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`
	t.Cleanup(func() { delete(toolSchemas, "blocking_probe") })
	model := &scriptedModel{steps: []provider.Response{{
		Text:       "running both probes",
		StopReason: provider.StopToolUse,
		ToolCalls: []provider.ToolCall{
			{ID: "resume-c1", Name: "blocking_probe", Arguments: json.RawMessage(`{"id":"first"}`)},
			{ID: "resume-c2", Name: "blocking_probe", Arguments: json.RawMessage(`{"id":"second"}`)},
		},
	}}}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type promptResult struct {
		turn RuntimeTurn
		err  error
	}
	done := make(chan promptResult, 1)
	go func() {
		turn, promptErr := runtime.Prompt(ctx, started.Session.ID, "run both probes")
		done <- promptResult{turn: turn, err: promptErr}
	}()
	<-toolStarted
	cancel()
	result := <-done
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Prompt() error = %v, want context cancellation", result.err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	resumeModel := &scriptedModel{steps: []provider.Response{{Text: "resumed safely", StopReason: provider.StopEndTurn}}}
	resumer, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: resumeModel, ModelID: "test/model",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumer.Close() })
	resumed, err := resumer.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertInterruptedResultCount(t, resumed.Transcript, "resume-c2", 1)
	select {
	case executed := <-executions:
		if executed != "first" {
			t.Fatalf("executed tool = %q, want first", executed)
		}
	default:
		t.Fatal("first tool was not executed")
	}
	select {
	case unexpected := <-executions:
		t.Fatalf("second sibling executed before recovery: %q", unexpected)
	default:
	}

	if _, err := resumer.Prompt(context.Background(), resumed.Session.ID, "continue after recovery"); err != nil {
		t.Fatalf("resumed Prompt() error = %v", err)
	}
	if len(resumeModel.requests) != 1 {
		t.Fatalf("resumed provider requests = %d, want one", len(resumeModel.requests))
	}
	messages := resumeModel.requests[0].Messages
	_, err = historyMessages(resumed.Transcript)
	if err != nil {
		t.Fatalf("historyMessages() error = %v", err)
	}
	groupIndex := -1
	var callIDs []string
	for index, message := range messages {
		for _, block := range message.Blocks {
			if block.Type == provider.BlockToolCall && block.ToolCall != nil {
				groupIndex = index
				callIDs = append(callIDs, block.ToolCall.ID)
			}
		}
	}
	if !slices.Equal(callIDs, []string{"resume-c1", "resume-c2"}) {
		t.Fatalf("resumed assistant call IDs = %v", callIDs)
	}
	if groupIndex < 0 || groupIndex+1 >= len(messages) ||
		messages[groupIndex+1].Role != provider.RoleTool || len(messages[groupIndex+1].Blocks) != 2 {
		t.Fatalf("resumed provider history does not contain one assistant group followed by both results: %+v", messages)
	}
}

func TestResumeRejectsDuplicateDurableToolCallIDs(t *testing.T) {
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
		{SessionID: started.Session.ID, Kind: TranscriptToolCall, ToolName: "first", Metadata: map[string]any{"tool_call_id": "reused-call"}},
		{SessionID: started.Session.ID, Kind: TranscriptToolResult, ToolName: "first", Metadata: map[string]any{"tool_call_id": "reused-call"}},
		{SessionID: started.Session.ID, Kind: TranscriptToolCall, ToolName: "second", Metadata: map[string]any{"tool_call_id": "reused-call"}},
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
	if _, err := resumer.Resume(context.Background(), started.Session.ID); err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("Resume() error = %v, want fail-closed duplicate ID rejection", err)
	}
}

func TestResumeRecoversLegacyToolCallWithoutIDOnce(t *testing.T) {
	dir := t.TempDir()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	legacyCall, err := runtime.transcript(started.Session).Append(TranscriptEntry{
		SessionID: started.Session.ID, Kind: TranscriptToolCall, ToolName: "legacy_probe", Input: map[string]any{"value": "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	legacyID := syntheticLegacyToolCallID(legacyCall, 1) // the start status is entry zero
	resumer, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := resumer.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatalf("first Resume() error = %v", err)
	}
	assertInterruptedResultCount(t, resumed.Transcript, legacyID, 1)
	if _, err := historyMessages(resumed.Transcript); err != nil {
		t.Fatalf("legacy resumed history error = %v", err)
	}
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
		t.Fatalf("second Resume() error = %v", err)
	}
	assertInterruptedResultCount(t, resumedAgain.Transcript, legacyID, 1)
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

func TestResumeRepairsMissingInterruptedAudit(t *testing.T) {
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
		{
			SessionID: started.Session.ID,
			Kind:      TranscriptToolCall,
			ToolName:  "interrupted",
			Input:     map[string]any{"value": "input"},
			Metadata:  map[string]any{"tool_call_id": "repair-call"},
		},
		{
			SessionID: started.Session.ID,
			Kind:      TranscriptToolResult,
			ToolName:  "interrupted",
			Output:    map[string]any{"summary": "interrupted", "interrupted": true},
			Metadata:  map[string]any{"tool_call_id": "repair-call", "recovered": true},
		},
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
	if _, err := resumer.Resume(context.Background(), started.Session.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := readToolCallAuditIDs(started.Session.ToolCallsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ids["repair-call"] || len(ids) != 1 {
		t.Fatalf("audit IDs = %v, want only repair-call", ids)
	}
}

func TestReadToolCallAuditIDsAcceptsLargeLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool-calls.jsonl")
	record := map[string]any{
		"tool_call_id": "large-call",
		"input":        strings.Repeat("x", 256*1024),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := readToolCallAuditIDs(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ids["large-call"] {
		t.Fatalf("audit IDs = %v, want large-call", ids)
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

func TestResumeRepairsOnlyInvalidUnterminatedTranscriptTail(t *testing.T) {
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
	f, err := os.OpenFile(started.Session.TranscriptPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"torn"`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
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
	var diagnostic bool
	for _, entry := range resumed.Transcript {
		if entry.Metadata["recovery"] == "torn_transcript_tail_discarded" {
			diagnostic = true
		}
	}
	if !diagnostic {
		t.Fatal("resume did not persist a torn-tail recovery diagnostic")
	}
	data, err := os.ReadFile(started.Session.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `{"id":"torn"`) || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("repaired transcript tail = %q", data)
	}
}

func TestRepairTrailingTranscriptCompletesValidFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	data := []byte(`{"id":"1","session_id":"s","kind":"status"}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	repaired, err := repairTrailingTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if repaired {
		t.Fatal("valid unterminated final record was incorrectly reported as discarded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := append(data, '\n')
	if string(got) != string(want) {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestResumeRepairsToolCallAuditTail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tail       string
		diagnostic bool
	}{
		{name: "valid final line", tail: `{"tool_call_id":"valid"}`},
		{name: "torn final line", tail: `{"tool_call_id":"torn"`, diagnostic: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if err := os.WriteFile(started.Session.ToolCallsPath, []byte(tc.tail), 0o600); err != nil {
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
			var diagnostic bool
			for _, entry := range resumed.Transcript {
				if entry.Metadata["recovery"] == "torn_tool_call_audit_tail_discarded" {
					diagnostic = true
				}
			}
			if diagnostic != tc.diagnostic {
				t.Fatalf("recovery diagnostic = %v, want %v", diagnostic, tc.diagnostic)
			}
			data, err := os.ReadFile(started.Session.ToolCallsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.diagnostic && !strings.HasSuffix(string(data), "\n") {
				t.Fatalf("repaired audit = %q, want newline-terminated JSONL", data)
			}
			if tc.diagnostic && strings.Contains(string(data), `"torn"`) {
				t.Fatalf("repaired audit still contains torn tail: %q", data)
			}
		})
	}
}

func TestResumeRejectsMiddleToolCallAuditCorruption(t *testing.T) {
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
	content := "{\"tool_call_id\":\"first\"}\nnot-json\n{\"tool_call_id\":\"last\"}\n"
	if err := os.WriteFile(started.Session.ToolCallsPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	resumer, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumer.Close() })
	if _, err := resumer.Resume(context.Background(), started.Session.ID); err == nil ||
		!strings.Contains(err.Error(), "tool-call audit") ||
		!strings.Contains(err.Error(), "line 2") {
		t.Fatalf("Resume() error = %v, want middle audit corruption at line 2", err)
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
