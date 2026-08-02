package operate

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// This is the durable nominal-resume lane: a governed tool turn is completed,
// the owning runtime is closed, a distinct runtime resumes the same session,
// and the next real model request must receive the exact provider-valid past.
func TestNominalResumeCarriesCompletedToolTurnIntoNextModelRequest(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	firstModel := &scriptedModel{steps: []provider.Response{
		{
			Text:       "I will inspect the available workers.",
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID: "call_qa57", Name: "list_workers", Arguments: json.RawMessage(`{}`),
			}},
		},
		{Text: "First turn complete.", StopReason: provider.StopEndTurn},
	}}
	first, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: firstModel, ModelID: "test/resume-first",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime(first) error = %v", err)
	}
	started, err := first.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		_ = first.Close()
		t.Fatalf("Start() error = %v", err)
	}
	consumeSuccessfulTurn(t, first, started.Session.ID, "inspect the workers", "prompt")

	before, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		_ = first.Close()
		t.Fatalf("ReadTranscript(before close) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	secondModel := &scriptedModel{steps: []provider.Response{{
		Text: "Second turn complete.", StopReason: provider.StopEndTurn,
	}}}
	second, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: secondModel, ModelID: "test/resume-second",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	resumed, err := second.Resume(context.Background(), started.Session.ID)
	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed.Session.ID != started.Session.ID {
		t.Fatalf("resumed session = %q, want %q", resumed.Session.ID, started.Session.ID)
	}
	if !reflect.DeepEqual(resumed.Transcript, before) {
		t.Fatalf("nominal resume mutated history:\n got  %+v\n want %+v", resumed.Transcript, before)
	}

	consumeSuccessfulTurn(t, second, resumed.Session.ID, "continue the inspection", "follow_up")
	if len(secondModel.requests) != 1 {
		t.Fatalf("second model requests = %d, want exactly 1", len(secondModel.requests))
	}
	assertNominalResumedProviderHistory(t, secondModel.requests[0].Messages)

	after, err := ReadTranscript(resumed.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript(after resume) error = %v", err)
	}
	if len(after) != len(before)+2 {
		t.Fatalf("transcript entries = %d, want %d; %+v", len(after), len(before)+2, after)
	}
	if !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("second turn changed durable prefix:\n got  %+v\n want %+v", after[:len(before)], before)
	}
	if after[len(before)].Kind != TranscriptUser || after[len(before)].Text != "continue the inspection" ||
		after[len(before)+1].Kind != TranscriptAssistant || after[len(before)+1].Text != "Second turn complete." {
		t.Fatalf("second turn suffix = %+v", after[len(before):])
	}
	calls, results := 0, 0
	for _, entry := range after {
		if entry.SessionID != resumed.Session.ID {
			t.Fatalf("entry session = %q, want %q: %+v", entry.SessionID, resumed.Session.ID, entry)
		}
		if metaString(entry.Metadata, "tool_call_id") != "call_qa57" {
			continue
		}
		switch entry.Kind {
		case TranscriptToolCall:
			calls++
		case TranscriptToolResult:
			results++
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("durable call_qa57 cardinality = calls:%d results:%d", calls, results)
	}
}

func consumeSuccessfulTurn(t *testing.T, runtime *AgentRuntime, sessionID, text, kind string) {
	t.Helper()
	stream, err := runtime.RunTurn(context.Background(), sessionID, text, kind)
	if err != nil {
		t.Fatalf("RunTurn(%q) error = %v", text, err)
	}
	done, streamErrors := 0, 0
	for event := range stream {
		if event.Kind == StreamError {
			streamErrors++
		}
		if event.Kind == StreamDone {
			done++
			if event.Err != nil {
				t.Fatalf("RunTurn(%q) done error = %v", text, event.Err)
			}
		}
	}
	if done != 1 || streamErrors != 0 {
		t.Fatalf("RunTurn(%q) terminal events = done:%d errors:%d", text, done, streamErrors)
	}
}

func assertNominalResumedProviderHistory(t *testing.T, messages []provider.Message) {
	t.Helper()
	if len(messages) != 5 {
		t.Fatalf("resumed provider messages = %d, want 5: %+v", len(messages), messages)
	}
	for i, message := range messages {
		if err := message.Validate(); err != nil {
			t.Fatalf("resumed provider message %d invalid: %v", i, err)
		}
	}
	roles := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleTool, provider.RoleAssistant, provider.RoleUser}
	for i, role := range roles {
		if messages[i].Role != role {
			t.Fatalf("message %d role = %q, want %q", i, messages[i].Role, role)
		}
	}
	if messages[0].Text() != "inspect the workers" ||
		messages[1].Text() != "I will inspect the available workers." ||
		messages[3].Text() != "First turn complete." ||
		messages[4].Text() != "continue the inspection" {
		t.Fatalf("resumed text history = %#v", messages)
	}
	if len(messages[1].Blocks) != 2 {
		t.Fatalf("resumed assistant tool message has %d blocks, want text + one call: %+v", len(messages[1].Blocks), messages[1])
	}
	var call *provider.ToolCall
	toolCalls := 0
	for _, block := range messages[1].Blocks {
		if block.Type == provider.BlockToolCall {
			toolCalls++
			call = block.ToolCall
		}
	}
	if toolCalls != 1 || call == nil || call.ID != "call_qa57" || call.Name != "list_workers" || string(call.Arguments) != `{}` {
		t.Fatalf("resumed tool call = %+v", call)
	}
	if len(messages[2].Blocks) != 1 || messages[2].Blocks[0].ToolResult == nil {
		t.Fatalf("resumed tool result message = %+v", messages[2])
	}
	result := messages[2].Blocks[0].ToolResult
	if result.ToolCallID != "call_qa57" || result.Name != "list_workers" || result.IsError {
		t.Fatalf("resumed tool result = %+v", result)
	}
}
