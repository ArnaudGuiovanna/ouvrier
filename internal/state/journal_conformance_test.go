package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// assertJournalConformance is the shared durable-run journal conformance suite
// exercised by the in-memory, SQLite, and Postgres backends so all
// implementations stay in sync. newStore must return a fresh store per call.
func assertJournalConformance(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()

	t.Run("JournalRoundTripAndUpsert", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		created := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
		journal := RunJournal{
			ExecID:      "exec_1",
			PlanKey:     "http:POST /tickets",
			PlanHash:    "abc123",
			TriggerKind: "http",
			Input:       `{"title":"broken"}`,
			CreatedAt:   created,
		}
		if err := store.SaveRunJournal(ctx, journal); err != nil {
			t.Fatalf("SaveRunJournal returned error: %v", err)
		}

		got, ok, err := store.RunJournal(ctx, "exec_1")
		if err != nil {
			t.Fatalf("RunJournal returned error: %v", err)
		}
		if !ok {
			t.Fatal("RunJournal ok = false for saved journal, want true")
		}
		if got.ExecID != journal.ExecID || got.PlanKey != journal.PlanKey ||
			got.PlanHash != journal.PlanHash || got.TriggerKind != journal.TriggerKind ||
			got.Input != journal.Input || !got.CreatedAt.Equal(created) {
			t.Fatalf("RunJournal = %+v, want round-trip of %+v", got, journal)
		}

		// Upsert: a second save for the same exec overwrites.
		journal.PlanHash = "def456"
		if err := store.SaveRunJournal(ctx, journal); err != nil {
			t.Fatalf("SaveRunJournal upsert returned error: %v", err)
		}
		got, _, err = store.RunJournal(ctx, "exec_1")
		if err != nil {
			t.Fatalf("RunJournal after upsert returned error: %v", err)
		}
		if got.PlanHash != "def456" {
			t.Fatalf("upserted plan hash = %q, want %q", got.PlanHash, "def456")
		}

		if _, ok, err := store.RunJournal(ctx, "absent"); err != nil || ok {
			t.Fatalf("RunJournal(absent) = ok=%v err=%v, want false, nil", ok, err)
		}
	})

	t.Run("JournalDefaultsCreatedAt", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		if err := store.SaveRunJournal(ctx, RunJournal{ExecID: "exec_now"}); err != nil {
			t.Fatalf("SaveRunJournal returned error: %v", err)
		}
		got, ok, err := store.RunJournal(ctx, "exec_now")
		if err != nil || !ok {
			t.Fatalf("RunJournal ok=%v err=%v", ok, err)
		}
		if got.CreatedAt.IsZero() {
			t.Fatal("CreatedAt not defaulted for zero-timestamp journal write")
		}
	})

	t.Run("RunJournalsListsSortedByCreatedAt", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		empty, err := store.RunJournals(ctx)
		if err != nil {
			t.Fatalf("RunJournals returned error: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("RunJournals on fresh store = %+v, want empty", empty)
		}

		base := time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)
		for _, journal := range []RunJournal{
			{ExecID: "exec_b", CreatedAt: base.Add(2 * time.Minute)},
			{ExecID: "exec_a", CreatedAt: base},
			{ExecID: "exec_c", CreatedAt: base.Add(time.Minute)},
		} {
			if err := store.SaveRunJournal(ctx, journal); err != nil {
				t.Fatalf("SaveRunJournal(%s) returned error: %v", journal.ExecID, err)
			}
		}
		journals, err := store.RunJournals(ctx)
		if err != nil {
			t.Fatalf("RunJournals returned error: %v", err)
		}
		if len(journals) != 3 {
			t.Fatalf("RunJournals = %d entries, want 3", len(journals))
		}
		wantOrder := []string{"exec_a", "exec_c", "exec_b"}
		for i, want := range wantOrder {
			if journals[i].ExecID != want {
				t.Fatalf("RunJournals[%d] = %q, want %q (order %v)", i, journals[i].ExecID, want, wantOrder)
			}
		}
	})

	t.Run("JournalInputRedactedAtRest", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		if err := store.SaveRunJournal(ctx, RunJournal{
			ExecID: "exec_secret",
			Input:  "deploy with token=raw-token api_key=raw-key Authorization: Bearer raw-bearer",
		}); err != nil {
			t.Fatalf("SaveRunJournal returned error: %v", err)
		}
		got, ok, err := store.RunJournal(ctx, "exec_secret")
		if err != nil || !ok {
			t.Fatalf("RunJournal ok=%v err=%v", ok, err)
		}
		want := "deploy with token=[REDACTED] api_key=[REDACTED] Authorization: [REDACTED]"
		if got.Input != want {
			t.Fatalf("journal input = %q, want redacted %q", got.Input, want)
		}
	})

	t.Run("CheckpointRoundTripOrderedAndUpsert", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		completed := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
		for _, checkpoint := range []RunCheckpoint{
			{ExecID: "exec_1", StepIndex: 2, Output: "step two", CompletedAt: completed.Add(2 * time.Second)},
			{ExecID: "exec_1", StepIndex: 0, Output: "step zero", CompletedAt: completed},
			{ExecID: "exec_1", StepIndex: 1, Output: "step one", CompletedAt: completed.Add(time.Second)},
			{ExecID: "exec_2", StepIndex: 0, Output: "other exec", CompletedAt: completed},
		} {
			if err := store.SaveRunCheckpoint(ctx, checkpoint); err != nil {
				t.Fatalf("SaveRunCheckpoint returned error: %v", err)
			}
		}

		checkpoints, err := store.RunCheckpoints(ctx, "exec_1")
		if err != nil {
			t.Fatalf("RunCheckpoints returned error: %v", err)
		}
		if len(checkpoints) != 3 {
			t.Fatalf("RunCheckpoints = %d entries, want 3: %+v", len(checkpoints), checkpoints)
		}
		for i, want := range []string{"step zero", "step one", "step two"} {
			if checkpoints[i].StepIndex != i || checkpoints[i].Output != want {
				t.Fatalf("checkpoint[%d] = %+v, want step %d output %q", i, checkpoints[i], i, want)
			}
			if !checkpoints[i].CompletedAt.Equal(completed.Add(time.Duration(i) * time.Second)) {
				t.Fatalf("checkpoint[%d] CompletedAt = %s, want preserved timestamp", i, checkpoints[i].CompletedAt)
			}
		}

		// Upsert on (exec_id, step_index): re-checkpointing a step overwrites
		// (e.g. a resumed suspended step).
		if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{ExecID: "exec_1", StepIndex: 1, Output: "step one again"}); err != nil {
			t.Fatalf("SaveRunCheckpoint upsert returned error: %v", err)
		}
		checkpoints, err = store.RunCheckpoints(ctx, "exec_1")
		if err != nil {
			t.Fatalf("RunCheckpoints after upsert returned error: %v", err)
		}
		if len(checkpoints) != 3 || checkpoints[1].Output != "step one again" {
			t.Fatalf("checkpoints after upsert = %+v, want overwritten step 1", checkpoints)
		}

		if none, err := store.RunCheckpoints(ctx, "absent"); err != nil || len(none) != 0 {
			t.Fatalf("RunCheckpoints(absent) = %v, %v, want empty, nil", none, err)
		}
	})

	t.Run("CheckpointOutputRedactedAtRest", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{
			ExecID:    "exec_secret",
			StepIndex: 0,
			Output:    "result token=raw-token api_key=raw-key Authorization: Bearer raw-bearer",
		}); err != nil {
			t.Fatalf("SaveRunCheckpoint returned error: %v", err)
		}
		checkpoints, err := store.RunCheckpoints(ctx, "exec_secret")
		if err != nil || len(checkpoints) != 1 {
			t.Fatalf("RunCheckpoints = %v, %v, want 1 row", checkpoints, err)
		}
		want := "result token=[REDACTED] api_key=[REDACTED] Authorization: [REDACTED]"
		if checkpoints[0].Output != want {
			t.Fatalf("checkpoint output = %q, want redacted %q", checkpoints[0].Output, want)
		}
	})

	t.Run("ToolIntentBeginCompleteRoundTrip", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		started := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
		intent := ToolIntent{
			ExecID:     "exec_1",
			ToolCallID: "call_1",
			StepIndex:  1,
			ToolName:   "send_email",
			Effect:     "side_effecting",
			IdemKey:    "args:deadbeef",
			StartedAt:  started,
		}
		if err := store.BeginToolIntent(ctx, intent); err != nil {
			t.Fatalf("BeginToolIntent returned error: %v", err)
		}

		intents, err := store.ToolIntents(ctx, "exec_1")
		if err != nil {
			t.Fatalf("ToolIntents returned error: %v", err)
		}
		if len(intents) != 1 {
			t.Fatalf("ToolIntents = %d entries, want 1", len(intents))
		}
		got := intents[0]
		if got.ToolCallID != "call_1" || got.StepIndex != 1 || got.ToolName != "send_email" ||
			got.Effect != "side_effecting" || got.IdemKey != "args:deadbeef" || !got.StartedAt.Equal(started) {
			t.Fatalf("intent = %+v, want round-trip of %+v", got, intent)
		}
		if !got.CompletedAt.IsZero() {
			t.Fatalf("open intent CompletedAt = %s, want zero", got.CompletedAt)
		}

		if err := store.CompleteToolIntent(ctx, "exec_1", "call_1"); err != nil {
			t.Fatalf("CompleteToolIntent returned error: %v", err)
		}
		intents, err = store.ToolIntents(ctx, "exec_1")
		if err != nil || len(intents) != 1 {
			t.Fatalf("ToolIntents after complete = %v, %v", intents, err)
		}
		if intents[0].CompletedAt.IsZero() {
			t.Fatal("completed intent CompletedAt is zero, want stamped")
		}
	})

	t.Run("ToolIntentBeginUpsertReopens", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		if err := store.BeginToolIntent(ctx, ToolIntent{
			ExecID: "exec_1", ToolCallID: "call_1", ToolName: "send_email",
		}); err != nil {
			t.Fatalf("BeginToolIntent returned error: %v", err)
		}
		if err := store.CompleteToolIntent(ctx, "exec_1", "call_1"); err != nil {
			t.Fatalf("CompleteToolIntent returned error: %v", err)
		}
		// Re-begin under the same key (retry of the same call id): the row is
		// re-opened rather than erroring.
		if err := store.BeginToolIntent(ctx, ToolIntent{
			ExecID: "exec_1", ToolCallID: "call_1", ToolName: "send_email", Effect: "idempotent",
		}); err != nil {
			t.Fatalf("BeginToolIntent re-begin returned error: %v", err)
		}
		intents, err := store.ToolIntents(ctx, "exec_1")
		if err != nil || len(intents) != 1 {
			t.Fatalf("ToolIntents = %v, %v, want 1 row", intents, err)
		}
		if !intents[0].CompletedAt.IsZero() {
			t.Fatalf("re-begun intent CompletedAt = %s, want re-opened zero", intents[0].CompletedAt)
		}
		if intents[0].Effect != "idempotent" {
			t.Fatalf("re-begun intent effect = %q, want overwritten %q", intents[0].Effect, "idempotent")
		}
	})

	t.Run("ToolIntentsOrderedByStartedAt", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		base := time.Date(2026, 6, 12, 11, 0, 0, 0, time.UTC)
		for _, intent := range []ToolIntent{
			{ExecID: "exec_1", ToolCallID: "call_b", ToolName: "tool_b", StartedAt: base.Add(time.Second)},
			{ExecID: "exec_1", ToolCallID: "call_a", ToolName: "tool_a", StartedAt: base},
			{ExecID: "exec_2", ToolCallID: "call_x", ToolName: "tool_x", StartedAt: base},
		} {
			if err := store.BeginToolIntent(ctx, intent); err != nil {
				t.Fatalf("BeginToolIntent returned error: %v", err)
			}
		}
		intents, err := store.ToolIntents(ctx, "exec_1")
		if err != nil {
			t.Fatalf("ToolIntents returned error: %v", err)
		}
		if len(intents) != 2 || intents[0].ToolCallID != "call_a" || intents[1].ToolCallID != "call_b" {
			t.Fatalf("ToolIntents order = %+v, want [call_a call_b]", intents)
		}
	})

	t.Run("CompleteMissingToolIntentFails", func(t *testing.T) {
		store := newStore(t)
		err := store.CompleteToolIntent(context.Background(), "exec_1", "call_missing")
		if err == nil {
			t.Fatal("CompleteToolIntent on missing intent returned nil error, want failure")
		}
	})

	t.Run("PruneRunJournalRemovesAllRows", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		seedJournalFixture(t, store, "exec_1", time.Now().UTC())
		seedJournalFixture(t, store, "exec_2", time.Now().UTC())

		if err := store.PruneRunJournal(ctx, "exec_1"); err != nil {
			t.Fatalf("PruneRunJournal returned error: %v", err)
		}
		if _, ok, err := store.RunJournal(ctx, "exec_1"); err != nil || ok {
			t.Fatalf("RunJournal after prune = ok=%v err=%v, want gone", ok, err)
		}
		if checkpoints, err := store.RunCheckpoints(ctx, "exec_1"); err != nil || len(checkpoints) != 0 {
			t.Fatalf("RunCheckpoints after prune = %v, %v, want empty", checkpoints, err)
		}
		if intents, err := store.ToolIntents(ctx, "exec_1"); err != nil || len(intents) != 0 {
			t.Fatalf("ToolIntents after prune = %v, %v, want empty", intents, err)
		}
		// Other executions are untouched.
		if _, ok, err := store.RunJournal(ctx, "exec_2"); err != nil || !ok {
			t.Fatalf("RunJournal(exec_2) after prune = ok=%v err=%v, want kept", ok, err)
		}
		// Pruning an absent execution is an idempotent no-op.
		if err := store.PruneRunJournal(ctx, "exec_1"); err != nil {
			t.Fatalf("second PruneRunJournal returned error: %v, want idempotent nil", err)
		}
	})

	t.Run("PruneRunJournalsBeforeRemovesExpired", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		cutoff := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
		seedJournalFixture(t, store, "exec_old", cutoff.Add(-time.Hour))
		seedJournalFixture(t, store, "exec_older", cutoff.Add(-2*time.Hour))
		seedJournalFixture(t, store, "exec_new", cutoff.Add(time.Hour))

		pruned, err := store.PruneRunJournalsBefore(ctx, cutoff)
		if err != nil {
			t.Fatalf("PruneRunJournalsBefore returned error: %v", err)
		}
		if len(pruned) != 2 {
			t.Fatalf("pruned = %v, want exec_old and exec_older", pruned)
		}
		got := map[string]bool{}
		for _, execID := range pruned {
			got[execID] = true
		}
		if !got["exec_old"] || !got["exec_older"] {
			t.Fatalf("pruned = %v, want exec_old and exec_older", pruned)
		}
		if _, ok, err := store.RunJournal(ctx, "exec_new"); err != nil || !ok {
			t.Fatalf("RunJournal(exec_new) = ok=%v err=%v, want kept", ok, err)
		}
		if checkpoints, err := store.RunCheckpoints(ctx, "exec_old"); err != nil || len(checkpoints) != 0 {
			t.Fatalf("RunCheckpoints(exec_old) = %v, %v, want cascaded prune", checkpoints, err)
		}
		if intents, err := store.ToolIntents(ctx, "exec_old"); err != nil || len(intents) != 0 {
			t.Fatalf("ToolIntents(exec_old) = %v, %v, want cascaded prune", intents, err)
		}

		again, err := store.PruneRunJournalsBefore(ctx, cutoff)
		if err != nil || len(again) != 0 {
			t.Fatalf("second PruneRunJournalsBefore = %v, %v, want no-op", again, err)
		}
	})

	t.Run("ValidationErrors", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		if err := store.SaveRunJournal(ctx, RunJournal{}); err == nil {
			t.Fatal("SaveRunJournal accepted empty exec id, want error")
		}
		if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{StepIndex: 0}); err == nil {
			t.Fatal("SaveRunCheckpoint accepted empty exec id, want error")
		}
		if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{ExecID: "exec_1", StepIndex: -1}); err == nil {
			t.Fatal("SaveRunCheckpoint accepted negative step index, want error")
		}
		if err := store.BeginToolIntent(ctx, ToolIntent{ExecID: "exec_1", ToolCallID: "call_1"}); err == nil {
			t.Fatal("BeginToolIntent accepted empty tool name, want error")
		}
		if err := store.BeginToolIntent(ctx, ToolIntent{ExecID: "exec_1", ToolName: "tool"}); err == nil {
			t.Fatal("BeginToolIntent accepted empty tool call id, want error")
		}
		if err := store.CompleteToolIntent(ctx, "", "call_1"); err == nil {
			t.Fatal("CompleteToolIntent accepted empty exec id, want error")
		}
		if err := store.PruneRunJournal(ctx, "  "); err == nil {
			t.Fatal("PruneRunJournal accepted blank exec id, want error")
		}
	})

	t.Run("HonorsCanceledContext", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.SaveRunJournal(ctx, RunJournal{ExecID: "exec_1"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveRunJournal error = %v, want context.Canceled", err)
		}
		if _, _, err := store.RunJournal(ctx, "exec_1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunJournal error = %v, want context.Canceled", err)
		}
		if _, err := store.RunJournals(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunJournals error = %v, want context.Canceled", err)
		}
		if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{ExecID: "exec_1"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveRunCheckpoint error = %v, want context.Canceled", err)
		}
		if _, err := store.RunCheckpoints(ctx, "exec_1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("RunCheckpoints error = %v, want context.Canceled", err)
		}
		if err := store.BeginToolIntent(ctx, ToolIntent{ExecID: "exec_1", ToolCallID: "call_1", ToolName: "tool"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("BeginToolIntent error = %v, want context.Canceled", err)
		}
		if err := store.CompleteToolIntent(ctx, "exec_1", "call_1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("CompleteToolIntent error = %v, want context.Canceled", err)
		}
		if _, err := store.ToolIntents(ctx, "exec_1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("ToolIntents error = %v, want context.Canceled", err)
		}
		if err := store.PruneRunJournal(ctx, "exec_1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("PruneRunJournal error = %v, want context.Canceled", err)
		}
		if _, err := store.PruneRunJournalsBefore(ctx, time.Now()); !errors.Is(err, context.Canceled) {
			t.Fatalf("PruneRunJournalsBefore error = %v, want context.Canceled", err)
		}
	})
}

// seedJournalFixture writes one journal row plus a checkpoint and an open
// intent for execID, so prune assertions can verify cascading deletes.
func seedJournalFixture(t *testing.T, store Store, execID string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveRunJournal(ctx, RunJournal{
		ExecID:      execID,
		PlanKey:     "http:POST /tickets",
		PlanHash:    "hash",
		TriggerKind: "http",
		Input:       "input for " + execID,
		CreatedAt:   createdAt,
	}); err != nil {
		t.Fatalf("seed SaveRunJournal(%s) returned error: %v", execID, err)
	}
	if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{
		ExecID:    execID,
		StepIndex: 0,
		Output:    "output for " + execID,
	}); err != nil {
		t.Fatalf("seed SaveRunCheckpoint(%s) returned error: %v", execID, err)
	}
	if err := store.BeginToolIntent(ctx, ToolIntent{
		ExecID:     execID,
		ToolCallID: "call_" + execID,
		ToolName:   "send_email",
		Effect:     "side_effecting",
		IdemKey:    "args:hash",
	}); err != nil {
		t.Fatalf("seed BeginToolIntent(%s) returned error: %v", execID, err)
	}
}

func TestMemoryStoreJournalConformance(t *testing.T) {
	assertJournalConformance(t, func(t *testing.T) Store {
		return NewMemoryStore()
	})
}

func TestSQLiteStoreJournalConformance(t *testing.T) {
	assertJournalConformance(t, func(t *testing.T) Store {
		return newTestSQLiteStore(t, t.TempDir()+"/state.db")
	})
}

func TestPostgresStoreJournalConformance(t *testing.T) {
	assertJournalConformance(t, func(t *testing.T) Store {
		return newTestPostgresStore(t)
	})
}

func TestSQLiteStoreJournalPersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestSQLiteStore(t, path)
	seedJournalFixture(t, store, "exec_1", time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC))
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	journal, ok, err := reopened.RunJournal(context.Background(), "exec_1")
	if err != nil || !ok {
		t.Fatalf("RunJournal after reopen ok=%v err=%v", ok, err)
	}
	if journal.Input != "input for exec_1" {
		t.Fatalf("reopened journal input = %q", journal.Input)
	}
	checkpoints, err := reopened.RunCheckpoints(context.Background(), "exec_1")
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("RunCheckpoints after reopen = %v, %v", checkpoints, err)
	}
	intents, err := reopened.ToolIntents(context.Background(), "exec_1")
	if err != nil || len(intents) != 1 {
		t.Fatalf("ToolIntents after reopen = %v, %v", intents, err)
	}
	if !intents[0].CompletedAt.IsZero() {
		t.Fatalf("reopened open intent CompletedAt = %s, want zero", intents[0].CompletedAt)
	}
}

// TestSQLiteStoreJournalRedactsRawRowsOnRead pins the read-side redaction
// backstop: secrets written by an older binary (or out-of-band) never leave
// the store unredacted.
func TestSQLiteStoreJournalRedactsRawRowsOnRead(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestSQLiteStore(t, path)
	raw := "raw token=raw-token api_key=raw-key Authorization: Bearer raw-bearer"
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO ouvrier_run_journal (
		exec_id, plan_key, plan_hash, trigger_kind, input, created_at
	) VALUES (?, ?, ?, ?, ?, ?)`,
		"exec_raw", "key", "hash", "http", raw,
		formatSQLiteTime(time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)),
	); err != nil {
		t.Fatalf("raw journal insert returned error: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO ouvrier_run_checkpoints (
		exec_id, step_index, output, completed_at
	) VALUES (?, ?, ?, ?)`,
		"exec_raw", 0, raw,
		formatSQLiteTime(time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)),
	); err != nil {
		t.Fatalf("raw checkpoint insert returned error: %v", err)
	}

	want := "raw token=[REDACTED] api_key=[REDACTED] Authorization: [REDACTED]"
	journal, ok, err := store.RunJournal(context.Background(), "exec_raw")
	if err != nil || !ok {
		t.Fatalf("RunJournal ok=%v err=%v", ok, err)
	}
	if journal.Input != want {
		t.Fatalf("raw journal input read back = %q, want %q", journal.Input, want)
	}
	checkpoints, err := store.RunCheckpoints(context.Background(), "exec_raw")
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("RunCheckpoints = %v, %v", checkpoints, err)
	}
	if checkpoints[0].Output != want {
		t.Fatalf("raw checkpoint output read back = %q, want %q", checkpoints[0].Output, want)
	}
}

// TestSQLiteJournalNoRawSecretBytesAtRest opens the database file directly
// and asserts the raw secret never reaches disk through the journal write
// path.
func TestSQLiteJournalNoRawSecretBytesAtRest(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestSQLiteStore(t, path)
	ctx := context.Background()
	if err := store.SaveRunJournal(ctx, RunJournal{
		ExecID: "exec_secret",
		Input:  "payload api_key=super-secret-credential-value",
	}); err != nil {
		t.Fatalf("SaveRunJournal returned error: %v", err)
	}
	if err := store.SaveRunCheckpoint(ctx, RunCheckpoint{
		ExecID:    "exec_secret",
		StepIndex: 0,
		Output:    "result token=super-secret-credential-value",
	}); err != nil {
		t.Fatalf("SaveRunCheckpoint returned error: %v", err)
	}

	var journalHits, checkpointHits int
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ouvrier_run_journal WHERE input LIKE '%super-secret-credential-value%'`).Scan(&journalHits); err != nil {
		t.Fatalf("raw journal scan returned error: %v", err)
	}
	if err := store.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ouvrier_run_checkpoints WHERE output LIKE '%super-secret-credential-value%'`).Scan(&checkpointHits); err != nil {
		t.Fatalf("raw checkpoint scan returned error: %v", err)
	}
	if journalHits != 0 || checkpointHits != 0 {
		t.Fatalf("raw secret found at rest: journal rows = %d, checkpoint rows = %d, want 0", journalHits, checkpointHits)
	}
	if !strings.Contains(redactedJournalInput(t, store), "[REDACTED]") {
		t.Fatal("stored journal input does not carry the redaction marker")
	}
}

func redactedJournalInput(t *testing.T, store *SQLiteStore) string {
	t.Helper()
	var input string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT input FROM ouvrier_run_journal WHERE exec_id = 'exec_secret'`).Scan(&input); err != nil {
		t.Fatalf("read stored journal input: %v", err)
	}
	return input
}
