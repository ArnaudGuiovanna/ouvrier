package operate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type scriptedModel struct {
	steps  []provider.Response
	i      int
	deltas int
}

func (m *scriptedModel) Complete(_ context.Context, req provider.Request, onDelta func(string)) (provider.Response, error) {
	if len(req.Tools) == 0 {
		return provider.Response{}, nil
	}
	if onDelta != nil {
		onDelta("working ")
		m.deltas++
	}
	resp := m.steps[m.i]
	m.i++
	return resp, nil
}

func writeMinimalWorker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		"pip.yaml":            "name: demo\n",
		"main.go":             "package main\n\nfunc main() {}\n",
		"ouvrier.worker.json": `{"name":"demo","events":["POST /tickets"],"outcomes":["triage"]}` + "\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestAgentLoopExecutesToolsThenFinishes(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	model := &scriptedModel{steps: []provider.Response{
		{
			Text:       "I'll list the workers.",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{
				{ID: "call_1", Name: "list_workers", Arguments: json.RawMessage(`{}`)},
			},
		},
		{
			Text:       "Found 1 worker; nothing else to do.",
			StopReason: provider.StopEndTurn,
		},
	}}

	rt, err := NewAgentRuntime(RuntimeOptions{
		Dir:     dir,
		Driver:  ManualDriver{},
		Model:   model,
		ModelID: "test/model",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "list the workers please", "prompt")
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}

	var sawToolStart, sawToolEnd, sawFinalAssistant, sawDone, sawDelta bool
	for ev := range ch {
		switch ev.Kind {
		case StreamAssistantDelta:
			sawDelta = true
		case StreamToolStart:
			if ev.Entry != nil && ev.Entry.ToolName == "list_workers" {
				sawToolStart = true
			}
		case StreamToolEnd:
			if ev.Entry != nil && ev.Entry.ToolName == "list_workers" {
				sawToolEnd = true
			}
		case StreamAssistant:
			if ev.Entry != nil && ev.Entry.Text == "Found 1 worker; nothing else to do." {
				sawFinalAssistant = true
			}
		case StreamDone:
			sawDone = true
		}
	}

	if !sawDelta {
		t.Error("expected streaming deltas")
	}
	if !sawToolStart || !sawToolEnd {
		t.Errorf("expected list_workers tool start/end, got start=%v end=%v", sawToolStart, sawToolEnd)
	}
	if !sawFinalAssistant {
		t.Error("expected final assistant message")
	}
	if !sawDone {
		t.Error("expected done event")
	}
	if model.i != 2 {
		t.Errorf("expected 2 model steps, got %d", model.i)
	}

	// Transcript persisted the loop: user, tool call, tool result, assistants.
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var kinds []TranscriptKind
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Contains(kinds, TranscriptToolCall) || !slices.Contains(kinds, TranscriptToolResult) {
		t.Fatalf("transcript missing tool call/result: %v", kinds)
	}
	if !slices.Contains(kinds, TranscriptUser) {
		t.Fatalf("transcript missing user entry: %v", kinds)
	}
}

func TestAgentLoopPersistsToolCallID(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	model := &scriptedModel{steps: []provider.Response{
		{
			Text:       "listing",
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{{ID: "call_abc", Name: "list_workers", Arguments: json.RawMessage(`{}`)}},
		},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}

	rt, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "list", "prompt")
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	for range ch {
	}

	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var callID, resultID string
	for _, e := range entries {
		switch e.Kind {
		case TranscriptToolCall:
			callID, _ = e.Metadata["tool_call_id"].(string)
		case TranscriptToolResult:
			resultID, _ = e.Metadata["tool_call_id"].(string)
		}
	}
	if callID == "" || callID != resultID {
		t.Fatalf("tool_call_id mismatch: call=%q result=%q", callID, resultID)
	}
	if callID != "call_abc" {
		t.Fatalf("expected model-supplied id call_abc, got %q", callID)
	}
}

func TestHistoryMessagesMultipleToolCallsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	model := &scriptedModel{steps: []provider.Response{
		{
			Text:       "I'll call two tools.",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "list_workers", Arguments: json.RawMessage(`{}`)},
				{ID: "c2", Name: "read_ouvrier_api", Arguments: json.RawMessage(`{}`)},
			},
		},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}

	rt, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "do two things", "prompt")
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	for range ch {
	}

	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	msgs := historyMessages(entries)

	// Every reconstructed message must be provider-valid.
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			t.Fatalf("message %d invalid: %v\nfull history: %+v", i, err, msgs)
		}
	}
	// Both tool calls and both results must be present and correctly paired.
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolCall && b.ToolCall != nil {
				calls[b.ToolCall.ID] = true
			}
			if b.Type == provider.BlockToolResult && b.ToolResult != nil {
				results[b.ToolResult.ToolCallID] = true
			}
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if !calls[id] || !results[id] {
			t.Fatalf("missing call/result for %s: call=%v result=%v\nhistory=%+v", id, calls[id], results[id], msgs)
		}
	}
	// No two adjacent assistant messages (would be invalid alternation).
	for i := 1; i < len(msgs); i++ {
		if msgs[i-1].Role == provider.RoleAssistant && msgs[i].Role == provider.RoleAssistant {
			t.Fatalf("two adjacent assistant messages at %d-%d: %+v", i-1, i, msgs)
		}
	}
}

func TestHistoryMessagesReplaysToolTurns(t *testing.T) {
	entries := []TranscriptEntry{
		{Kind: TranscriptUser, Text: "list the workers"},
		{Kind: TranscriptAssistant, Text: "I'll list them."},
		{Kind: TranscriptToolCall, ToolName: "list_workers", Input: map[string]any{}, Metadata: map[string]any{"tool_call_id": "c1"}},
		{Kind: TranscriptToolResult, ToolName: "list_workers", Output: map[string]any{"summary": "1 worker"}, Metadata: map[string]any{"tool_call_id": "c1"}},
		{Kind: TranscriptAssistant, Text: "Found 1 worker."},
		{Kind: TranscriptUser, Text: "now audit it"},
	}

	msgs := historyMessages(entries)

	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != provider.RoleUser {
		t.Fatalf("msg0 role = %q", msgs[0].Role)
	}
	var sawToolCall, sawToolResult bool
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolCall && b.ToolCall != nil && b.ToolCall.ID == "c1" {
				sawToolCall = true
			}
			if b.Type == provider.BlockToolResult && b.ToolResult != nil && b.ToolResult.ToolCallID == "c1" {
				sawToolResult = true
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("missing tool call/result in history: call=%v result=%v", sawToolCall, sawToolResult)
	}
	if msgs[len(msgs)-1].Role != provider.RoleUser || msgs[len(msgs)-1].Text() != "now audit it" {
		t.Fatalf("last message should be the new user prompt, got %+v", msgs[len(msgs)-1])
	}
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			t.Fatalf("msg %d invalid: %v", i, err)
		}
	}
}
