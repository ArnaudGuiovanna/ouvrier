package state

import (
	"context"
	"testing"
)

func assertIdempotencyOutcomeConformance(t *testing.T, newStore func(*testing.T) Store) {
	t.Helper()
	outcomeStore := func(t *testing.T) interface {
		Store
		IdempotencyOutcomeStore
	} {
		t.Helper()
		store := newStore(t)
		outcomes, ok := store.(IdempotencyOutcomeStore)
		if !ok {
			t.Fatalf("store %T does not implement IdempotencyOutcomeStore", store)
		}
		return struct {
			Store
			IdempotencyOutcomeStore
		}{Store: store, IdempotencyOutcomeStore: outcomes}
	}

	t.Run("PendingCannotBeStolen", func(t *testing.T) {
		store := outcomeStore(t)
		if _, reserved, err := store.ReserveIdempotency(context.Background(), "key-1", "exec-1"); err != nil || !reserved {
			t.Fatalf("initial reservation reserved=%v err=%v", reserved, err)
		}
		owner, reserved, err := store.ReserveIdempotency(context.Background(), "key-1", "exec-2")
		if err != nil || reserved || owner != "exec-1" {
			t.Fatalf("contender owner=%q reserved=%v err=%v, want pending owner", owner, reserved, err)
		}
		record, ok, err := store.Idempotency(context.Background(), "key-1")
		if err != nil || !ok || record.Outcome != IdempotencyPending || record.ExecID != "exec-1" {
			t.Fatalf("record=%+v ok=%v err=%v", record, ok, err)
		}
	})

	t.Run("FailedReservationCanBeRetried", func(t *testing.T) {
		store := outcomeStore(t)
		if _, reserved, err := store.ReserveIdempotency(context.Background(), "key-1", "exec-1"); err != nil || !reserved {
			t.Fatalf("initial reservation reserved=%v err=%v", reserved, err)
		}
		if err := store.ResolveIdempotency(context.Background(), "key-1", "exec-1", IdempotencyFailed); err != nil {
			t.Fatalf("ResolveIdempotency failed: %v", err)
		}
		if _, reserved, err := store.ReserveIdempotency(context.Background(), "key-1", "exec-2"); err != nil || !reserved {
			t.Fatalf("retry reservation reserved=%v err=%v", reserved, err)
		}
		record, ok, err := store.Idempotency(context.Background(), "key-1")
		if err != nil || !ok || record.Outcome != IdempotencyPending || record.ExecID != "exec-2" {
			t.Fatalf("retry record=%+v ok=%v err=%v", record, ok, err)
		}
		if !record.UpdatedAt.Equal(record.CreatedAt) {
			t.Fatalf("fresh retry timestamps differ: %+v", record)
		}
	})

	t.Run("SucceededReservationIsStable", func(t *testing.T) {
		store := outcomeStore(t)
		if _, reserved, err := store.ReserveIdempotency(context.Background(), "key-1", "exec-1"); err != nil || !reserved {
			t.Fatalf("initial reservation reserved=%v err=%v", reserved, err)
		}
		if err := store.ResolveIdempotency(context.Background(), "key-1", "exec-1", IdempotencySucceeded); err != nil {
			t.Fatalf("ResolveIdempotency succeeded: %v", err)
		}
		if err := store.ResolveIdempotency(context.Background(), "key-1", "exec-1", IdempotencySucceeded); err != nil {
			t.Fatalf("idempotent resolution returned error: %v", err)
		}
		owner, reserved, err := store.ReserveIdempotency(context.Background(), "key-1", "exec-2")
		if err != nil || reserved || owner != "exec-1" {
			t.Fatalf("post-success owner=%q reserved=%v err=%v", owner, reserved, err)
		}
		if err := store.ResolveIdempotency(context.Background(), "key-1", "exec-1", IdempotencyFailed); err == nil {
			t.Fatal("resolved success changed to failed")
		}
	})

	t.Run("ResolveByExecutionOnlyTouchesMatchingPendingPrefix", func(t *testing.T) {
		store := outcomeStore(t)
		for _, item := range []struct{ key, exec string }{
			{"trigger:http:1", "exec-1"},
			{"tool:send:1", "exec-1"},
			{"trigger:http:2", "exec-2"},
		} {
			if _, reserved, err := store.ReserveIdempotency(context.Background(), item.key, item.exec); err != nil || !reserved {
				t.Fatalf("reserve %s: reserved=%v err=%v", item.key, reserved, err)
			}
		}
		if err := store.ResolveIdempotencyByExecution(context.Background(), "exec-1", "trigger:", IdempotencyFailed); err != nil {
			t.Fatalf("ResolveIdempotencyByExecution returned error: %v", err)
		}
		want := map[string]IdempotencyOutcome{
			"trigger:http:1": IdempotencyFailed,
			"tool:send:1":    IdempotencyPending,
			"trigger:http:2": IdempotencyPending,
		}
		for key, outcome := range want {
			record, ok, err := store.Idempotency(context.Background(), key)
			if err != nil || !ok || record.Outcome != outcome {
				t.Fatalf("record %s=%+v ok=%v err=%v, want %s", key, record, ok, err, outcome)
			}
		}
	})

	t.Run("RejectsInvalidResolution", func(t *testing.T) {
		store := outcomeStore(t)
		if err := store.ResolveIdempotency(context.Background(), "", "exec", IdempotencySucceeded); err == nil {
			t.Fatal("blank key accepted")
		}
		if err := store.ResolveIdempotencyByExecution(context.Background(), "exec", "", IdempotencyPending); err == nil {
			t.Fatal("pending terminal outcome accepted")
		}
	})
}

func TestMemoryStoreIdempotencyOutcomeConformance(t *testing.T) {
	assertIdempotencyOutcomeConformance(t, func(*testing.T) Store { return NewMemoryStore() })
}

func TestSQLiteStoreIdempotencyOutcomeConformance(t *testing.T) {
	assertIdempotencyOutcomeConformance(t, func(t *testing.T) Store {
		return newTestSQLiteStore(t, t.TempDir()+"/state.db")
	})
}

func TestPostgresStoreIdempotencyOutcomeConformance(t *testing.T) {
	assertIdempotencyOutcomeConformance(t, func(t *testing.T) Store { return newTestPostgresStore(t) })
}
