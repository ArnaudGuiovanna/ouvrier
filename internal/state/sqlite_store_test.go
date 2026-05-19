package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runtimecore "ouvrier/internal/runtime"
)

func TestSQLiteStorePersistsExecutionAndSessionAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.db")
	started := time.Date(2026, 5, 18, 14, 0, 0, 123, time.UTC)
	completed := time.Date(2026, 5, 18, 14, 1, 0, 456, time.UTC)

	store := newTestSQLiteStore(t, path)
	err := store.SaveExecution(context.Background(), Execution{
		ExecID:      "exec_1",
		TraceID:     "trace_1",
		Status:      ExecutionCompleted,
		StartedAt:   started,
		CompletedAt: completed,
	})
	if err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
	err = store.SaveSession(context.Background(), runtimecore.Session{
		ExecID:          "exec_1",
		SessionID:       "sess_1",
		ParentSessionID: "sess_parent",
		TraceID:         "trace_1",
		Model:           "openai/gpt-5.1",
		StartedAt:       started,
		Budget:          runtimecore.Budget{MaxIterations: 7, MaxTokens: 4096, MaxCostUSD: 0.42, MaxWallClock: 2 * time.Minute},
	})
	if err != nil {
		t.Fatalf("SaveSession returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	gotExec, ok, err := reopened.Execution(context.Background(), "exec_1")
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if gotExec.Status != ExecutionCompleted || !gotExec.StartedAt.Equal(started) || !gotExec.CompletedAt.Equal(completed) {
		t.Fatalf("Execution = %+v", gotExec)
	}

	gotSession, ok, err := reopened.Session(context.Background(), "sess_1")
	if err != nil {
		t.Fatalf("Session returned error: %v", err)
	}
	if !ok {
		t.Fatal("Session ok = false, want true")
	}
	if gotSession.Model != "openai/gpt-5.1" || gotSession.ParentSessionID != "sess_parent" {
		t.Fatalf("Session = %+v", gotSession)
	}
	if gotSession.Budget.MaxIterations != 7 || gotSession.Budget.MaxTokens != 4096 || gotSession.Budget.MaxCostUSD != 0.42 || gotSession.Budget.MaxWallClock != 2*time.Minute {
		t.Fatalf("Session budget = %+v", gotSession.Budget)
	}
}

func TestSQLiteStoreListsExecutionsInDeterministicOrder(t *testing.T) {
	store := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "state.db"))
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

func TestSQLiteStoreReserveIdempotencyIsAtomic(t *testing.T) {
	store := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "state.db"))
	const contenders = 32

	type result struct {
		execID   string
		existing string
		reserved bool
		err      error
	}

	start := make(chan struct{})
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			execID := fmt.Sprintf("exec_%d", i)
			existing, reserved, err := store.ReserveIdempotency(context.Background(), "request-1", execID)
			results <- result{execID: execID, existing: existing, reserved: reserved, err: err}
		}(i)
	}

	close(start)
	wg.Wait()
	close(results)

	reservedCount := 0
	winner := ""
	seen := make([]result, 0, contenders)
	for got := range results {
		if got.err != nil {
			t.Fatalf("ReserveIdempotency returned error: %v", got.err)
		}
		seen = append(seen, got)
		if got.reserved {
			reservedCount++
			winner = got.execID
		}
	}
	if reservedCount != 1 {
		t.Fatalf("reserved count = %d, want 1", reservedCount)
	}
	for _, got := range seen {
		if !got.reserved && got.existing != winner {
			t.Fatalf("loser existing = %q, want %q", got.existing, winner)
		}
	}
}

func TestSQLiteStoreRecordsSchemaViolations(t *testing.T) {
	store := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "state.db"))
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

	if first.ID == 0 {
		t.Fatal("first ID = 0, want generated ID")
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

func TestSQLiteStoreHonorsCanceledContext(t *testing.T) {
	store := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "state.db"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := store.SaveExecution(ctx, Execution{ExecID: "exec_1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveExecution error = %v, want context.Canceled", err)
	}
}

func newTestSQLiteStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore returned error: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func executionIDs(executions []Execution) []string {
	ids := make([]string, 0, len(executions))
	for _, execution := range executions {
		ids = append(ids, execution.ExecID)
	}
	return ids
}
