package operate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessPatchFailureIsPersistedAsTerminalState(t *testing.T) {
	dir := writeWorkerFixture(t)
	gitInitAndCommit(t, dir)
	driverErr := errors.New("provider unavailable")
	harness, err := NewHarness(Options{
		Dir: dir,
		Driver: &fakeDriver{run: func(TurnRequest) error {
			return driverErr
		}},
	})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	session, ws, err := harness.Start(context.Background(), dir, "repair worker", "fake", "test")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if _, err := harness.PatchWorker(context.Background(), session, ws, "repair worker"); !errors.Is(err, driverErr) {
		t.Fatalf("PatchWorker() error = %v, want %v", err, driverErr)
	}
	assertPersistedFailureState(t, harness.Store, session.ID, StatusPatchFailed, "provider unavailable")
}

func TestHarnessAuditExecutionFailureIsPersistedAsTerminalState(t *testing.T) {
	dir := writeWorkerFixture(t)
	harness, err := NewHarness(Options{Dir: dir})
	if err != nil {
		t.Fatalf("NewHarness() error = %v", err)
	}
	session, _, err := harness.Start(context.Background(), dir, "audit worker", "manual", "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	missing := filepath.Join(dir, "missing-candidate")
	if _, err := harness.RunAudit(context.Background(), session, missing); err == nil {
		t.Fatal("RunAudit() error = nil, want missing candidate error")
	}
	assertPersistedFailureState(t, harness.Store, session.ID, StatusAuditFailed, "missing-candidate")
}

func assertPersistedFailureState(t *testing.T, store *Store, sessionID string, want Status, errorFragment string) {
	t.Helper()
	loaded, err := store.Load(sessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Status != want {
		t.Fatalf("status = %q, want %q", loaded.Status, want)
	}
	if !strings.Contains(loaded.LastError, errorFragment) {
		t.Fatalf("last_error = %q, want fragment %q", loaded.LastError, errorFragment)
	}
	if len(loaded.Transitions) == 0 || loaded.Transitions[len(loaded.Transitions)-1].To != want {
		t.Fatalf("last transition = %+v, want %q", loaded.Transitions, want)
	}
}
