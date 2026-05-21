package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"ouvrier/internal/events"
	runtimecore "ouvrier/internal/runtime"
)

func TestMemoryStoreSavesExecutionAndSessionSnapshots(t *testing.T) {
	store := NewMemoryStore()
	started := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC)
	execution := Execution{
		ExecID:    "exec_1",
		TraceID:   "trace_1",
		Status:    ExecutionRunning,
		StartedAt: started,
	}
	session := runtimecore.Session{
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Model:     "anthropic/claude-sonnet-4-6",
	}

	if err := store.SaveExecution(context.Background(), execution); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("SaveSession returned error: %v", err)
	}

	gotExec, ok, err := store.Execution(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if gotExec.Status != ExecutionRunning || !gotExec.StartedAt.Equal(started) {
		t.Fatalf("Execution = %+v", gotExec)
	}

	gotSession, ok, err := store.Session(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("Session returned error: %v", err)
	}
	if !ok {
		t.Fatal("Session ok = false, want true")
	}
	if gotSession.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("Session = %+v", gotSession)
	}
}

func TestMemoryStoreListsExecutionsInDeterministicOrder(t *testing.T) {
	store := NewMemoryStore()
	base := time.Date(2026, 5, 18, 14, 0, 0, 0, time.UTC)
	for _, execution := range []Execution{
		{ExecID: "exec_c", TraceID: "trace_c", Status: ExecutionCompleted, StartedAt: base.Add(2 * time.Minute)},
		{ExecID: "exec_b", TraceID: "trace_b", Status: ExecutionRunning, StartedAt: base.Add(time.Minute)},
		{ExecID: "exec_a", TraceID: "trace_a", Status: ExecutionFailed, StartedAt: base.Add(time.Minute)},
	} {
		if err := store.SaveExecution(context.Background(), execution); err != nil {
			t.Fatalf("SaveExecution returned error: %v", err)
		}
	}

	executions, err := store.Executions(context.Background())
	if err != nil {
		t.Fatalf("Executions returned error: %v", err)
	}
	gotIDs := executionIDs(executions)
	wantIDs := []string{"exec_a", "exec_b", "exec_c"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
		t.Fatalf("execution IDs = %v, want %v", gotIDs, wantIDs)
	}
	if executions[1].Status != ExecutionRunning {
		t.Fatalf("second execution = %+v", executions[1])
	}
}

func TestMemoryStoreReserveIdempotencyKey(t *testing.T) {
	store := NewMemoryStore()

	existing, reserved, err := store.ReserveIdempotency(context.Background(), "request-1", "exec_1")
	if err != nil {
		t.Fatalf("ReserveIdempotency returned error: %v", err)
	}
	if !reserved || existing != "" {
		t.Fatalf("first reserve existing=%q reserved=%v, want empty true", existing, reserved)
	}

	existing, reserved, err = store.ReserveIdempotency(context.Background(), "request-1", "exec_2")
	if err != nil {
		t.Fatalf("ReserveIdempotency returned error: %v", err)
	}
	if reserved || existing != "exec_1" {
		t.Fatalf("second reserve existing=%q reserved=%v, want exec_1 false", existing, reserved)
	}
}

func TestMemoryStoreRecordsEvents(t *testing.T) {
	store := NewMemoryStore()
	at := time.Date(2026, 5, 18, 15, 0, 0, 0, time.UTC)

	first, err := store.AddEvent(context.Background(), events.Event{
		At:        at,
		Kind:      events.EventBeforeTool,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		TraceID:   "trace_1",
		Payload: map[string]any{
			"tool":    "load_ticket",
			"api_key": "secret-key",
		},
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	_, err = store.AddEvent(context.Background(), events.Event{Kind: events.EventAfterLLM, ExecID: "exec_2"})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}

	if first.ID != 1 {
		t.Fatalf("first ID = %d, want 1", first.ID)
	}
	recorded, err := store.Events(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("events = %d, want 1", len(recorded))
	}
	if recorded[0].Kind != events.EventToolCallStarted || !recorded[0].At.Equal(at) {
		t.Fatalf("event = %+v", recorded[0])
	}
	if recorded[0].Payload["api_key"] != "[REDACTED]" || recorded[0].Payload["tool"] != "load_ticket" {
		t.Fatalf("payload = %+v, want redacted secret and visible tool", recorded[0].Payload)
	}
}

func TestMemoryStorePreservesEventIDsAndFiltersEventsSince(t *testing.T) {
	store := NewMemoryStore()

	first, err := store.AddEvent(context.Background(), events.Event{
		ID:     7,
		Kind:   events.EventSessionStarted,
		ExecID: "exec_1",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	second, err := store.AddEvent(context.Background(), events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_1",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	_, err = store.AddEvent(context.Background(), events.Event{
		Kind:   events.EventLLMCallCompleted,
		ExecID: "exec_2",
	})
	if err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}

	if first.ID != 7 {
		t.Fatalf("first ID = %d, want preserved ID 7", first.ID)
	}
	if second.ID != 8 {
		t.Fatalf("second ID = %d, want generated ID after preserved ID", second.ID)
	}
	recorded, err := store.EventsSince(context.Background(), "exec_1", 7)
	if err != nil {
		t.Fatalf("EventsSince returned error: %v", err)
	}
	if len(recorded) != 1 || recorded[0].ID != 8 {
		t.Fatalf("events since 7 = %+v, want exec_1 event 8", recorded)
	}
}

func TestMemoryStoreRecordsSchemaViolations(t *testing.T) {
	store := NewMemoryStore()
	at := time.Date(2026, 5, 18, 15, 0, 0, 0, time.UTC)

	first, err := store.AddSchemaViolation(context.Background(), SchemaViolation{
		ExecID:     "exec_1",
		SessionID:  "sess_1",
		SchemaName: "Triage",
		Error:      "missing field status",
		At:         at,
	})
	if err != nil {
		t.Fatalf("AddSchemaViolation returned error: %v", err)
	}
	_, err = store.AddSchemaViolation(context.Background(), SchemaViolation{ExecID: "exec_2"})
	if err != nil {
		t.Fatalf("AddSchemaViolation returned error: %v", err)
	}

	if first.ID != 1 {
		t.Fatalf("first ID = %d, want 1", first.ID)
	}
	violations, err := store.SchemaViolations(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(violations))
	}
	if violations[0].Error != "missing field status" || !violations[0].At.Equal(at) {
		t.Fatalf("violation = %+v", violations[0])
	}
}

func TestMemoryStoreHonorsCanceledContext(t *testing.T) {
	store := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.SaveExecution(ctx, Execution{ExecID: "exec_1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveExecution error = %v, want context.Canceled", err)
	}
}

func TestMemoryStoreConcurrentSessionWrites(t *testing.T) {
	store := NewMemoryStore()
	const total = 64

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.SaveSession(context.Background(), runtimecore.Session{
				ExecID:    "exec_1",
				SessionID: fmt.Sprintf("sess_%d", i),
				TraceID:   "trace_1",
			})
			if err != nil {
				t.Errorf("SaveSession returned error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	sessions, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions returned error: %v", err)
	}
	if len(sessions) != total {
		t.Fatalf("sessions = %d, want %d", len(sessions), total)
	}
}
