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
