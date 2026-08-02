package operate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type scriptedModel struct {
	steps    []provider.Response
	requests []provider.Request
	i        int
	deltas   int
}

type closableAgentModel struct {
	closed int
}

type abortablePendingModel struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	calls   int
	aborts  int
	pending bool
}

type oversizedStreamingModel struct {
	mu        sync.Mutex
	cancelled bool
}

type oversizedTextModel struct{}

func (*closableAgentModel) Complete(context.Context, provider.Request, func(string)) (provider.Response, error) {
	return provider.Response{Text: "done", StopReason: provider.StopEndTurn}, nil
}

func (m *closableAgentModel) Close() error {
	m.closed++
	return nil
}

func (m *abortablePendingModel) Complete(_ context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	if call == 1 {
		m.pending = true
		cancel := m.cancel
		m.mu.Unlock()
		cancel()
		return provider.Response{
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{{ID: "pending-call", Name: "list_workers", Arguments: json.RawMessage(`{}`)}},
		}, nil
	}
	stale := m.pending
	m.mu.Unlock()
	if stale {
		return provider.Response{}, errors.New("stale provider turn was reused")
	}
	return provider.Response{Text: "fresh turn complete", StopReason: provider.StopEndTurn}, nil
}

func (m *abortablePendingModel) AbortTurn(context.Context) error {
	m.mu.Lock()
	m.pending = false
	m.aborts++
	m.mu.Unlock()
	return nil
}

func (m *abortablePendingModel) AbortCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.aborts
}

func (m *oversizedStreamingModel) Complete(ctx context.Context, _ provider.Request, onDelta func(string)) (provider.Response, error) {
	onDelta(strings.Repeat("x", maxAgentModelTextBytes))
	onDelta("overflow")
	m.mu.Lock()
	m.cancelled = ctx.Err() != nil
	m.mu.Unlock()
	return provider.Response{}, ctx.Err()
}

func (m *oversizedStreamingModel) WasCancelled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancelled
}

func (*oversizedTextModel) Complete(context.Context, provider.Request, func(string)) (provider.Response, error) {
	return provider.Response{Text: strings.Repeat("x", maxAgentModelTextBytes+1), StopReason: provider.StopEndTurn}, nil
}

func (m *scriptedModel) Complete(_ context.Context, req provider.Request, onDelta func(string)) (provider.Response, error) {
	m.requests = append(m.requests, req)
	if len(req.Tools) == 0 {
		return provider.Response{}, nil
	}
	if m.i >= len(m.steps) {
		last := ""
		if len(req.Messages) > 0 {
			last = req.Messages[len(req.Messages)-1].Text()
		}
		if len(last) > 512 {
			last = last[:512] + "...[truncated]"
		}
		return provider.Response{}, fmt.Errorf("scripted model exhausted after %d response(s); provider request %d was unexpected (last message: %q)", len(m.steps), len(m.requests), last)
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

func TestAgentRuntimeCloseClosesModelTransportOnce(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &closableAgentModel{}
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if model.closed != 1 {
		t.Fatalf("model Close calls = %d, want exactly 1", model.closed)
	}
}

func TestAgentLoopAbortsProviderStateWhenCancelledAfterToolRequest(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	model := &abortablePendingModel{cancel: cancel}
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/stateful"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	first, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if _, err := runtime.Prompt(ctx, first.Session.ID, "list the worker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Prompt() error = %v", err)
	}
	if aborts := model.AbortCount(); aborts != 1 {
		t.Fatalf("AbortTurn calls = %d, want one", aborts)
	}

	second, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	turn, err := runtime.Prompt(context.Background(), second.Session.ID, "explain this worker")
	if err != nil {
		t.Fatalf("fresh Prompt() error = %v", err)
	}
	if turn.Final != "fresh turn complete" {
		t.Fatalf("fresh Prompt() final = %q", turn.Final)
	}
}

func TestAgentLoopBoundsStreamingAndCompletedModelText(t *testing.T) {
	tests := []struct {
		name  string
		model AgentModel
		want  string
	}{
		{name: "stream", model: &oversizedStreamingModel{}, want: "model streamed more than"},
		{name: "completed", model: &oversizedTextModel{}, want: "model response text exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMinimalWorker(t, dir)
			runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: tt.model, ModelID: "test/bounds"})
			if err != nil {
				t.Fatalf("NewAgentRuntime() error = %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			_, err = runtime.Prompt(context.Background(), started.Session.ID, "inspect the worker")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Prompt() error = %v", err)
			}
		})
	}
	streaming := tests[0].model.(*oversizedStreamingModel)
	if !streaming.WasCancelled() {
		t.Fatal("stream overflow did not cancel the provider context")
	}
}

func TestToolResultContentRemainsBoundedValidJSON(t *testing.T) {
	result := ToolResult{Summary: "large result", Data: map[string]any{"text": strings.Repeat("\\\"\x01", 20_000)}}
	content := toolResultContent(result, nil)
	if len(content) > maxModelToolResultBytes {
		t.Fatalf("tool result bytes = %d, want <= %d", len(content), maxModelToolResultBytes)
	}
	if !json.Valid([]byte(content)) {
		t.Fatalf("bounded tool result is invalid JSON: %q", content)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["truncated"] != true || envelope["summary"] != "large result" {
		t.Fatalf("bounded envelope = %+v", envelope)
	}

	replayed := toolResultContentFromOutput(map[string]any{"summary": "replayed", "text": strings.Repeat("x", 20_000)})
	if len(replayed) > maxModelToolResultBytes || !json.Valid([]byte(replayed)) {
		t.Fatalf("replayed result is not bounded valid JSON: bytes=%d", len(replayed))
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
	msgs, err := historyMessages(entries)
	if err != nil {
		t.Fatalf("historyMessages() error = %v", err)
	}

	// Every reconstructed message must be provider-valid.
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			t.Fatalf("message %d invalid: %v\nfull history: %+v", i, err, msgs)
		}
	}
	// Both tool calls and both results must be present and correctly paired.
	calls := map[string]bool{}
	results := map[string]bool{}
	groupMessages := 0
	groupIndex := -1
	for messageIndex, m := range msgs {
		callsInMessage := 0
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolCall && b.ToolCall != nil {
				calls[b.ToolCall.ID] = true
				callsInMessage++
			}
			if b.Type == provider.BlockToolResult && b.ToolResult != nil {
				results[b.ToolResult.ToolCallID] = true
			}
		}
		if callsInMessage > 0 {
			groupMessages++
			groupIndex = messageIndex
			if callsInMessage != 2 {
				t.Fatalf("assistant tool-call group has %d calls, want 2: %+v", callsInMessage, m)
			}
		}
	}
	if groupMessages != 1 {
		t.Fatalf("assistant tool-call messages = %d, want one durable group: %+v", groupMessages, msgs)
	}
	for _, id := range []string{"c1", "c2"} {
		if !calls[id] || !results[id] {
			t.Fatalf("missing call/result for %s: call=%v result=%v\nhistory=%+v", id, calls[id], results[id], msgs)
		}
	}
	if groupIndex+1 >= len(msgs) || msgs[groupIndex+1].Role != provider.RoleTool || len(msgs[groupIndex+1].Blocks) != 2 {
		t.Fatalf("group results are not one two-block tool message after assistant calls: %+v", msgs)
	}

	var transcriptGroupID string
	var groupedCalls int
	for _, entry := range entries {
		switch entry.Kind {
		case TranscriptAssistant:
			if id := metaString(entry.Metadata, toolCallGroupIDKey); id != "" {
				transcriptGroupID = id
				if count, ok := metaInt(entry.Metadata, toolCallGroupCountKey); !ok || count != 2 {
					t.Fatalf("assistant group count = %d/%v, want 2/true", count, ok)
				}
			}
		case TranscriptToolCall:
			if metaString(entry.Metadata, toolCallGroupIDKey) == transcriptGroupID && transcriptGroupID != "" {
				groupedCalls++
			}
		}
	}
	if transcriptGroupID == "" || groupedCalls != 2 {
		t.Fatalf("durable transcript group = %q with %d calls, want non-empty/2", transcriptGroupID, groupedCalls)
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

	msgs, err := historyMessages(entries)
	if err != nil {
		t.Fatalf("historyMessages() error = %v", err)
	}

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

func TestHistoryMessagesSynthesizesStableLegacyToolCallID(t *testing.T) {
	entries := []TranscriptEntry{
		{ID: "legacy-user", Kind: TranscriptUser, Text: "inspect"},
		{ID: "legacy-assistant", Kind: TranscriptAssistant, Text: "checking"},
		{ID: "legacy-call-entry", SessionID: "legacy-session", Kind: TranscriptToolCall, ToolName: "list_workers", Input: map[string]any{}},
		{ID: "legacy-result-entry", SessionID: "legacy-session", Kind: TranscriptToolResult, ToolName: "list_workers", Output: map[string]any{"summary": "done"}},
	}
	first, err := historyMessages(entries)
	if err != nil {
		t.Fatalf("first historyMessages() error = %v", err)
	}
	second, err := historyMessages(entries)
	if err != nil {
		t.Fatalf("second historyMessages() error = %v", err)
	}
	callID := func(messages []provider.Message) (string, string) {
		var call, result string
		for _, message := range messages {
			for _, block := range message.Blocks {
				if block.ToolCall != nil {
					call = block.ToolCall.ID
				}
				if block.ToolResult != nil {
					result = block.ToolResult.ToolCallID
				}
			}
		}
		return call, result
	}
	firstCall, firstResult := callID(first)
	secondCall, secondResult := callID(second)
	if firstCall == "" || !strings.HasPrefix(firstCall, "legacy_") || firstCall != firstResult || firstCall != secondCall || secondCall != secondResult {
		t.Fatalf("legacy IDs first=%q/%q second=%q/%q, want one deterministic paired ID", firstCall, firstResult, secondCall, secondResult)
	}

	grouped := []TranscriptEntry{{
		ID: "new-grouped-call", Kind: TranscriptToolCall, ToolName: "list_workers",
		Metadata: map[string]any{toolCallGroupIDKey: "new-group", toolCallGroupIndexKey: 0, toolCallGroupCountKey: 1},
	}}
	if _, err := historyMessages(grouped); err == nil || !strings.Contains(err.Error(), "has no stable id") {
		t.Fatalf("new grouped entry without ID error = %v", err)
	}
}

func TestValidateAgentToolCallIDBoundsOpaqueStableIDs(t *testing.T) {
	valid := []string{"call_123", "toolu-01H.xyz:7", strings.Repeat("x", maxAgentToolCallIDBytes)}
	for _, id := range valid {
		if err := validateAgentToolCallID(id); err != nil {
			t.Errorf("validateAgentToolCallID(%q) error = %v", id, err)
		}
	}
	invalid := []string{
		"",
		" call_1",
		"call 1",
		"call\n1",
		string([]byte{0xff}),
		strings.Repeat("x", maxAgentToolCallIDBytes+1),
	}
	for _, id := range invalid {
		if err := validateAgentToolCallID(id); err == nil {
			t.Errorf("validateAgentToolCallID(%q) error = nil", id)
		}
	}
}

func TestAgentLoopRejectsDuplicateToolCallIDWithinAssistantGroup(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{{
		StopReason: provider.StopToolUse,
		ToolCalls: []provider.ToolCall{
			{ID: "stable-call", Name: "list_workers", Arguments: json.RawMessage(`{}`)},
			{ID: "stable-call", Name: "list_workers", Arguments: json.RawMessage(`{}`)},
		},
	}}}
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "list workers twice")
	if err == nil || !strings.Contains(err.Error(), "duplicate model tool call id") {
		t.Fatalf("Prompt() error = %v, want duplicate group id rejection", err)
	}
	if !strings.Contains(turn.Final, "duplicate model tool call id") {
		t.Fatalf("turn final = %q", turn.Final)
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	callCount := 0
	for _, entry := range entries {
		if entry.Kind == TranscriptToolCall && metaString(entry.Metadata, "tool_call_id") == "stable-call" {
			callCount++
		}
	}
	if callCount != 0 {
		t.Fatalf("durable stable-call records = %d, want validation before persistence", callCount)
	}
}

func TestAgentLoopEnforcesTotalToolCallLimitAcrossGroups(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	executed := 0
	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{
		Name:       "probe",
		Governance: GovReadOnly,
		Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
			executed++
			return ToolResult{Summary: "ok"}, nil
		},
	})
	steps := make([]provider.Response, 0, 5)
	for group := 0; group < maxAgentToolCalls/maxAgentToolCallsPerStep; group++ {
		calls := make([]provider.ToolCall, 0, maxAgentToolCallsPerStep)
		for call := 0; call < maxAgentToolCallsPerStep; call++ {
			calls = append(calls, provider.ToolCall{
				ID: fmt.Sprintf("g%d-c%d", group, call), Name: "probe", Arguments: json.RawMessage(`{}`),
			})
		}
		steps = append(steps, provider.Response{StopReason: provider.StopToolUse, ToolCalls: calls})
	}
	steps = append(steps, provider.Response{
		StopReason: provider.StopToolUse,
		ToolCalls:  []provider.ToolCall{{ID: "one-too-many", Name: "probe", Arguments: json.RawMessage(`{}`)}},
	})
	model := &scriptedModel{steps: steps}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "probe bounded execution")
	if err == nil || !strings.Contains(err.Error(), "too many tools") {
		t.Fatalf("Prompt() error = %v, want total tool limit", err)
	}
	if !strings.Contains(turn.Final, "64 total limit") {
		t.Fatalf("turn.Final = %q, want total limit evidence", turn.Final)
	}
	if executed != maxAgentToolCalls {
		t.Fatalf("executed tools = %d, want exactly %d", executed, maxAgentToolCalls)
	}
}
