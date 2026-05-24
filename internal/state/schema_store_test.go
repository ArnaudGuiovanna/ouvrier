package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryStoreSchemaViolationPersistenceAssertions(t *testing.T) {
	store := NewMemoryStore()
	wantAll, wantExec := addSchemaViolationFixtures(t, store)

	assertSchemaViolationList(t, store, "", wantAll)
	assertSchemaViolationList(t, store, "exec_1", wantExec)
}

func TestSQLiteStoreSchemaViolationPersistenceAssertionsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store := newTestSQLiteStore(t, path)
	wantAll, wantExec := addSchemaViolationFixtures(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	assertSchemaViolationList(t, reopened, "", wantAll)
	assertSchemaViolationList(t, reopened, "exec_1", wantExec)
}

func TestMemoryStoreRedactsSchemaViolationErrorsBeforePersistence(t *testing.T) {
	assertSchemaViolationErrorRedaction(t, NewMemoryStore())
}

func TestSQLiteStoreRedactsSchemaViolationErrorsBeforePersistence(t *testing.T) {
	store := newTestSQLiteStore(t, filepath.Join(t.TempDir(), "state.db"))
	assertSchemaViolationErrorRedaction(t, store)
}

func addSchemaViolationFixtures(t *testing.T, store Store) ([]SchemaViolation, []SchemaViolation) {
	t.Helper()

	ctx := context.Background()
	base := time.Date(2026, 5, 18, 15, 0, 0, 123456789, time.UTC)
	fixtures := []SchemaViolation{
		{
			At:         base.Add(30 * time.Second),
			ExecID:     "exec_1",
			SessionID:  "sess_b",
			SchemaName: "Triage",
			Error:      "missing field status",
		},
		{
			At:         base.Add(10 * time.Second),
			ExecID:     "exec_2",
			SessionID:  "sess_c",
			SchemaName: "Enrichment",
			Error:      "wrong type for priority",
		},
		{
			At:         base.Add(20 * time.Second),
			ExecID:     "exec_1",
			SessionID:  "sess_a",
			SchemaName: "Repair",
			Error:      "enum mismatch",
		},
		{
			ExecID:     "exec_1",
			SessionID:  "sess_a",
			SchemaName: "FinalReply",
			Error:      "required field missing",
		},
	}

	added := make([]SchemaViolation, 0, len(fixtures))
	var previousID uint64
	for _, fixture := range fixtures {
		got, err := store.AddSchemaViolation(ctx, fixture)
		if err != nil {
			t.Fatalf("AddSchemaViolation returned error: %v", err)
		}
		if got.ID == 0 {
			t.Fatal("AddSchemaViolation returned ID 0, want generated ID")
		}
		if got.ID <= previousID {
			t.Fatalf("violation ID = %d after %d, want monotonic increase", got.ID, previousID)
		}
		if fixture.At.IsZero() {
			if got.At.IsZero() {
				t.Fatal("AddSchemaViolation returned zero At for fixture with no timestamp")
			}
		} else if !got.At.Equal(fixture.At) {
			t.Fatalf("violation At = %s, want preserved %s", got.At, fixture.At)
		}
		previousID = got.ID
		added = append(added, got)
	}

	return added, []SchemaViolation{added[0], added[2], added[3]}
}

func assertSchemaViolationErrorRedaction(t *testing.T, store Store) {
	t.Helper()

	ctx := context.Background()
	added, err := store.AddSchemaViolation(ctx, SchemaViolation{
		ExecID:     "exec_secret",
		SessionID:  "sess_secret",
		SchemaName: "SecretReply",
		Error:      "schema failed token=raw-token api_key=raw-key Authorization: Bearer raw-bearer",
	})
	if err != nil {
		t.Fatalf("AddSchemaViolation returned error: %v", err)
	}
	wantError := "schema failed token=[REDACTED] api_key=[REDACTED] Authorization: [REDACTED]"
	if added.Error != wantError {
		t.Fatalf("added violation error = %q, want %q", added.Error, wantError)
	}

	violations, err := store.SchemaViolations(ctx, "exec_secret")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1: %+v", len(violations), violations)
	}
	if violations[0].Error != wantError {
		t.Fatalf("persisted violation error = %q, want %q", violations[0].Error, wantError)
	}
}

func assertSchemaViolationList(t *testing.T, store Store, execID string, want []SchemaViolation) {
	t.Helper()

	got, err := store.SchemaViolations(context.Background(), execID)
	if err != nil {
		t.Fatalf("SchemaViolations(%q) returned error: %v", execID, err)
	}
	if len(got) != len(want) {
		t.Fatalf("SchemaViolations(%q) = %d violations, want %d: %+v", execID, len(got), len(want), got)
	}
	for i := range want {
		assertSchemaViolationEqual(t, execID, i, got[i], want[i])
	}
}

func assertSchemaViolationEqual(t *testing.T, execID string, index int, got, want SchemaViolation) {
	t.Helper()

	if got.ID != want.ID ||
		got.ExecID != want.ExecID ||
		got.SessionID != want.SessionID ||
		got.SchemaName != want.SchemaName ||
		got.Error != want.Error ||
		!got.At.Equal(want.At) {
		t.Fatalf("SchemaViolations(%q)[%d] = %+v, want %+v", execID, index, got, want)
	}
}
