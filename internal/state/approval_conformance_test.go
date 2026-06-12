package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// assertApprovalConformance is the shared pending-approval conformance suite
// exercised by both the in-memory and SQLite backends so the two
// implementations stay in sync. newStore must return a fresh store on each call.
func assertApprovalConformance(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()

	sample := func(id string) PendingApproval {
		return PendingApproval{
			ID:         id,
			ExecID:     "exec-1",
			SessionID:  "session-1",
			TraceID:    "trace-1",
			ToolName:   "wire_payment",
			ToolCallID: "call-1",
			ToolKind:   "function",
			Effect:     "side_effecting",
			Reason:     "tool requires explicit approval",
			ArgsHash:   "args:deadbeef",
			Status:     ApprovalPending,
		}
	}

	t.Run("SaveAndReadRoundTrip", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveApproval(context.Background(), sample("a-1")); err != nil {
			t.Fatalf("SaveApproval returned error: %v", err)
		}
		got, ok, err := store.Approval(context.Background(), "a-1")
		if err != nil {
			t.Fatalf("Approval returned error: %v", err)
		}
		if !ok {
			t.Fatal("Approval ok = false, want true")
		}
		if got.ToolName != "wire_payment" || got.ExecID != "exec-1" || got.Status != ApprovalPending {
			t.Fatalf("Approval = %+v, want round-trip of sample", got)
		}
		if got.CreatedAt.IsZero() {
			t.Fatal("Approval CreatedAt is zero, want a populated timestamp")
		}
		if got.ArgsHash != "args:deadbeef" {
			t.Fatalf("Approval args hash = %q, want round-trip of sample", got.ArgsHash)
		}
	})

	t.Run("ApprovalsForExecutionListsAllStatusesForOneExec", func(t *testing.T) {
		store := newStore(t)
		for _, id := range []string{"a-1", "a-2"} {
			if err := store.SaveApproval(context.Background(), sample(id)); err != nil {
				t.Fatalf("SaveApproval(%s) returned error: %v", id, err)
			}
		}
		other := sample("a-other")
		other.ExecID = "exec-2"
		if err := store.SaveApproval(context.Background(), other); err != nil {
			t.Fatalf("SaveApproval(other) returned error: %v", err)
		}
		if _, err := store.ResolveApproval(context.Background(), "a-1", ApprovalApproved, "ops"); err != nil {
			t.Fatalf("ResolveApproval returned error: %v", err)
		}

		approvals, err := store.ApprovalsForExecution(context.Background(), "exec-1")
		if err != nil {
			t.Fatalf("ApprovalsForExecution returned error: %v", err)
		}
		if len(approvals) != 2 {
			t.Fatalf("ApprovalsForExecution len = %d, want 2 (resolved rows included): %+v", len(approvals), approvals)
		}
		if approvals[0].ID != "a-1" || approvals[1].ID != "a-2" {
			t.Fatalf("ApprovalsForExecution order = [%s %s], want creation order [a-1 a-2]", approvals[0].ID, approvals[1].ID)
		}
		if approvals[0].Status != ApprovalApproved {
			t.Fatalf("ApprovalsForExecution a-1 status = %q, want approved", approvals[0].Status)
		}
		if approvals[0].ArgsHash != "args:deadbeef" {
			t.Fatalf("ApprovalsForExecution a-1 args hash = %q, want preserved through resolution", approvals[0].ArgsHash)
		}

		none, err := store.ApprovalsForExecution(context.Background(), "exec-absent")
		if err != nil || len(none) != 0 {
			t.Fatalf("ApprovalsForExecution(absent) = %v, %v, want empty", none, err)
		}
	})

	t.Run("MissingIDReturnsNotOK", func(t *testing.T) {
		store := newStore(t)
		_, ok, err := store.Approval(context.Background(), "absent")
		if err != nil {
			t.Fatalf("Approval returned error: %v", err)
		}
		if ok {
			t.Fatal("Approval ok = true for absent id, want false")
		}
	})

	t.Run("RequiresIDExecAndTool", func(t *testing.T) {
		store := newStore(t)
		bad := sample("")
		if err := store.SaveApproval(context.Background(), bad); err == nil {
			t.Fatal("SaveApproval accepted empty id, want error")
		}
		bad = sample("a-1")
		bad.ExecID = ""
		if err := store.SaveApproval(context.Background(), bad); err == nil {
			t.Fatal("SaveApproval accepted empty exec id, want error")
		}
		bad = sample("a-1")
		bad.ToolName = ""
		if err := store.SaveApproval(context.Background(), bad); err == nil {
			t.Fatal("SaveApproval accepted empty tool name, want error")
		}
	})

	t.Run("PendingApprovalsListsOnlyPendingSortedByCreation", func(t *testing.T) {
		store := newStore(t)
		for _, id := range []string{"a-1", "a-2", "a-3"} {
			if err := store.SaveApproval(context.Background(), sample(id)); err != nil {
				t.Fatalf("SaveApproval(%s) returned error: %v", id, err)
			}
		}
		if _, err := store.ResolveApproval(context.Background(), "a-2", ApprovalApproved, "ops"); err != nil {
			t.Fatalf("ResolveApproval returned error: %v", err)
		}
		pending, err := store.PendingApprovals(context.Background())
		if err != nil {
			t.Fatalf("PendingApprovals returned error: %v", err)
		}
		if len(pending) != 2 {
			t.Fatalf("PendingApprovals len = %d, want 2", len(pending))
		}
		if pending[0].ID != "a-1" || pending[1].ID != "a-3" {
			t.Fatalf("PendingApprovals order = [%s %s], want [a-1 a-3]", pending[0].ID, pending[1].ID)
		}
		for _, approval := range pending {
			if approval.Status != ApprovalPending {
				t.Fatalf("PendingApprovals returned non-pending status %q", approval.Status)
			}
		}
	})

	t.Run("ResolveApprovalRecordsDecision", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveApproval(context.Background(), sample("a-1")); err != nil {
			t.Fatalf("SaveApproval returned error: %v", err)
		}
		resolved, err := store.ResolveApproval(context.Background(), "a-1", ApprovalDenied, "alice")
		if err != nil {
			t.Fatalf("ResolveApproval returned error: %v", err)
		}
		if resolved.Status != ApprovalDenied {
			t.Fatalf("ResolveApproval status = %q, want denied", resolved.Status)
		}
		if resolved.DecidedBy != "alice" {
			t.Fatalf("ResolveApproval decided_by = %q, want alice", resolved.DecidedBy)
		}
		if resolved.DecidedAt.IsZero() {
			t.Fatal("ResolveApproval DecidedAt is zero, want a populated timestamp")
		}
		got, ok, err := store.Approval(context.Background(), "a-1")
		if err != nil || !ok {
			t.Fatalf("Approval ok=%v err=%v", ok, err)
		}
		if got.Status != ApprovalDenied {
			t.Fatalf("persisted status = %q, want denied", got.Status)
		}
	})

	t.Run("ResolveAlreadyDecidedIsRejected", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveApproval(context.Background(), sample("a-1")); err != nil {
			t.Fatalf("SaveApproval returned error: %v", err)
		}
		if _, err := store.ResolveApproval(context.Background(), "a-1", ApprovalApproved, "ops"); err != nil {
			t.Fatalf("first ResolveApproval returned error: %v", err)
		}
		if _, err := store.ResolveApproval(context.Background(), "a-1", ApprovalDenied, "ops"); err == nil {
			t.Fatal("second ResolveApproval succeeded, want error for already-decided approval")
		}
	})

	t.Run("ResolveMissingIsRejected", func(t *testing.T) {
		store := newStore(t)
		if _, err := store.ResolveApproval(context.Background(), "absent", ApprovalApproved, "ops"); err == nil {
			t.Fatal("ResolveApproval on missing id succeeded, want error")
		}
	})

	t.Run("ResolveRejectsNonTerminalStatus", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveApproval(context.Background(), sample("a-1")); err != nil {
			t.Fatalf("SaveApproval returned error: %v", err)
		}
		if _, err := store.ResolveApproval(context.Background(), "a-1", ApprovalPending, "ops"); err == nil {
			t.Fatal("ResolveApproval accepted pending status, want error")
		}
	})

	t.Run("RedactsSecretsBeforePersisting", func(t *testing.T) {
		store := newStore(t)
		approval := sample("a-1")
		approval.Reason = "token=sk-abcdefghijklmnopqrstuvwxyz0123456789"
		if err := store.SaveApproval(context.Background(), approval); err != nil {
			t.Fatalf("SaveApproval returned error: %v", err)
		}
		got, ok, err := store.Approval(context.Background(), "a-1")
		if err != nil || !ok {
			t.Fatalf("Approval ok=%v err=%v", ok, err)
		}
		if strings.Contains(got.Reason, "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Fatalf("Approval reason leaked secret: %q", got.Reason)
		}
	})

	t.Run("HonorsCanceledContext", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.SaveApproval(ctx, sample("a-1")); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveApproval error = %v, want context.Canceled", err)
		}
	})
}

// TestMemoryStoreApprovalResolveIsRaceSafe exercises concurrent resolution of
// the same pending approval to guarantee exactly one decision wins under -race.
func TestMemoryStoreApprovalResolveIsRaceSafe(t *testing.T) {
	store := NewMemoryStore()
	if err := store.SaveApproval(context.Background(), PendingApproval{
		ID:       "a-1",
		ExecID:   "exec-1",
		ToolName: "wire_payment",
		Status:   ApprovalPending,
	}); err != nil {
		t.Fatalf("SaveApproval returned error: %v", err)
	}

	const workers = 16
	var wins int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if _, err := store.ResolveApproval(context.Background(), "a-1", ApprovalApproved, "ops"); err == nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent ResolveApproval winners = %d, want exactly 1", wins)
	}
}

func TestMemoryStoreApprovalConformance(t *testing.T) {
	assertApprovalConformance(t, func(t *testing.T) Store {
		return NewMemoryStore()
	})
}

func TestSQLiteStoreApprovalConformance(t *testing.T) {
	assertApprovalConformance(t, func(t *testing.T) Store {
		return newTestSQLiteStore(t, t.TempDir()+"/state.db")
	})
}

func TestSQLiteStoreApprovalPersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestSQLiteStore(t, path)
	if err := store.SaveApproval(context.Background(), PendingApproval{
		ID:       "a-1",
		ExecID:   "exec-1",
		ToolName: "wire_payment",
		Status:   ApprovalPending,
	}); err != nil {
		t.Fatalf("SaveApproval returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	got, ok, err := reopened.Approval(context.Background(), "a-1")
	if err != nil {
		t.Fatalf("Approval returned error: %v", err)
	}
	if !ok || got.ToolName != "wire_payment" {
		t.Fatalf("Approval = %+v ok=%v after reopen; want persisted approval", got, ok)
	}
}
