package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
