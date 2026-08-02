package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestSameRuntimeSerializesCompleteSessionTurns(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := newSerializedTurnModel()
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/serialized-turns",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Prompt(context.Background(), started.Session.ID, "explain the worker")
		firstDone <- runErr
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("first turn did not reach the model")
	}

	secondCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, secondErr := runtime.Steer(secondCtx, started.Session.ID, "second instruction must not interleave")
	cancel()
	if !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("concurrent Steer() error = %v, want context deadline while turn lane is occupied", secondErr)
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Text, "second instruction") {
			t.Fatalf("waiting turn wrote before acquiring the session lane: %+v", entry)
		}
	}
	if calls := model.callCount(); calls != 1 {
		t.Fatalf("model calls while first turn blocked = %d, want one", calls)
	}

	close(model.release)
	select {
	case runErr := <-firstDone:
		if runErr != nil {
			t.Fatalf("first Prompt() error = %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not finish after release")
	}
	runtime.turnMu.Lock()
	lanes := len(runtime.turns)
	runtime.turnMu.Unlock()
	if lanes != 0 {
		t.Fatalf("idle session turn lanes = %d, want reclaimed", lanes)
	}
}

func TestRuntimeSerializesStatefulModelAcrossSessionsForWholeToolLoop(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	registry := NewToolRegistry()
	registry.Register(Tool{
		Name:       "hold_probe",
		Governance: GovReadOnly,
		Run: func(ctx context.Context, _ ToolEnv, _ map[string]any) (ToolResult, error) {
			close(toolStarted)
			select {
			case <-releaseTool:
				return ToolResult{Summary: "released"}, nil
			case <-ctx.Done():
				return ToolResult{}, ctx.Err()
			}
		},
	})
	model := &wholeLoopSerializedModel{}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/stateful", Tools: registry,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	first, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start(first) error = %v", err)
	}
	second, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start(second) error = %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Prompt(context.Background(), first.Session.ID, "inspect the first worker")
		firstDone <- runErr
	}()
	select {
	case <-toolStarted:
	case <-time.After(time.Second):
		t.Fatal("first turn did not reach its governed tool")
	}

	secondCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, secondErr := runtime.Prompt(secondCtx, second.Session.ID, "inspect the second worker")
	cancel()
	if !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("second Prompt() error = %v, want model-turn gate deadline", secondErr)
	}
	if calls := model.CallCount(); calls != 1 {
		t.Fatalf("model calls while first governed tool is active = %d, want one", calls)
	}

	close(releaseTool)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Prompt() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not finish after tool release")
	}
}

func TestInterruptCancelsActiveModelTurnBeforeRecordingBoundary(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := newSerializedTurnModel()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/interrupt"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() {
		close(model.release)
		_ = runtime.Close()
	})
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Prompt(context.Background(), started.Session.ID, "inspect the worker")
		promptDone <- runErr
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not reach model")
	}

	interrupted, err := runtime.Interrupt(context.Background(), started.Session.ID, "operator stopped this turn")
	if err != nil {
		t.Fatalf("Interrupt() error = %v", err)
	}
	if len(interrupted.Entries) != 1 || interrupted.Entries[0].Metadata["cancelled_active_turn"] != true {
		t.Fatalf("interrupt entry = %+v", interrupted.Entries)
	}
	select {
	case runErr := <-promptDone:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Prompt() error = %v, want cancellation", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Interrupt did not settle the active prompt")
	}
}

func TestRuntimeCloseCancelsAndJoinsActiveTurnBeforeUnlocking(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := newSerializedTurnModel()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/close"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Prompt(context.Background(), started.Session.ID, "inspect the worker")
		promptDone <- runErr
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("prompt did not reach model")
	}

	if err := runtime.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case runErr := <-promptDone:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Prompt() error = %v, want cancellation", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("active prompt did not report settlement after Close")
	}
	runtime.lockMu.Lock()
	locks := len(runtime.locks)
	runtime.lockMu.Unlock()
	if locks != 0 {
		t.Fatalf("writer locks after Close = %d", locks)
	}
	if _, err := runtime.Prompt(context.Background(), started.Session.ID, "must be rejected"); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("Prompt() after Close error = %v", err)
	}
	close(model.release)
}

func TestInitialPromptIsRedactedBeforeGoalPersistence(t *testing.T) {
	const secret = "goal-secret-value"
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Redactor: NewRedactor(secret)})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir, InitialPrompt: "construct with " + secret})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	data, err := os.ReadFile(started.Session.GoalPath)
	if err != nil {
		t.Fatalf("ReadFile(goal.md) error = %v", err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "***") {
		t.Fatalf("goal.md = %q, want secret redacted", data)
	}
}

type serializedTurnModel struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

type wholeLoopSerializedModel struct {
	mu    sync.Mutex
	calls int
}

func (m *wholeLoopSerializedModel) Complete(_ context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 1 {
		return provider.Response{
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{{ID: "hold-1", Name: "hold_probe", Arguments: json.RawMessage(`{}`)}},
		}, nil
	}
	return provider.Response{Text: "inspection complete", StopReason: provider.StopEndTurn}, nil
}

func (m *wholeLoopSerializedModel) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newSerializedTurnModel() *serializedTurnModel {
	return &serializedTurnModel{started: make(chan struct{}), release: make(chan struct{})}
}

func (m *serializedTurnModel) Complete(ctx context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	if call == 1 {
		close(m.started)
		select {
		case <-m.release:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
	}
	return provider.Response{Text: "worker explanation complete", StopReason: provider.StopEndTurn}, nil
}

func (m *serializedTurnModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
