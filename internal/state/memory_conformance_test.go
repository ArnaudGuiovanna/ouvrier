package state

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// assertMemoryConformance is the shared memory-store conformance suite exercised
// by both the in-memory and SQLite backends so the two implementations stay in
// sync. newStore must return a fresh store on each call.
func assertMemoryConformance(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()

	t.Run("SaveAndReadRoundTrip", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveMemory(context.Background(), "worker/agent", "preferred_tone", "concise"); err != nil {
			t.Fatalf("SaveMemory returned error: %v", err)
		}
		value, ok, err := store.Memory(context.Background(), "worker/agent", "preferred_tone")
		if err != nil {
			t.Fatalf("Memory returned error: %v", err)
		}
		if !ok || value != "concise" {
			t.Fatalf("Memory = %q, ok=%v; want %q, true", value, ok, "concise")
		}
	})

	t.Run("MissingKeyReturnsNotOK", func(t *testing.T) {
		store := newStore(t)
		_, ok, err := store.Memory(context.Background(), "worker/agent", "absent")
		if err != nil {
			t.Fatalf("Memory returned error: %v", err)
		}
		if ok {
			t.Fatal("Memory ok = true for absent key, want false")
		}
	})

	t.Run("OverwriteIsLastWriteWins", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveMemory(context.Background(), "scope", "k", "first"); err != nil {
			t.Fatalf("SaveMemory returned error: %v", err)
		}
		if err := store.SaveMemory(context.Background(), "scope", "k", "second"); err != nil {
			t.Fatalf("SaveMemory returned error: %v", err)
		}
		value, ok, err := store.Memory(context.Background(), "scope", "k")
		if err != nil || !ok {
			t.Fatalf("Memory ok=%v err=%v", ok, err)
		}
		if value != "second" {
			t.Fatalf("Memory = %q, want %q", value, "second")
		}
		entries, err := store.ListMemory(context.Background(), "scope")
		if err != nil {
			t.Fatalf("ListMemory returned error: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("ListMemory len = %d, want 1", len(entries))
		}
	})

	t.Run("ScopesAreIsolated", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveMemory(context.Background(), "worker-a/agent", "k", "a-value"); err != nil {
			t.Fatalf("SaveMemory returned error: %v", err)
		}
		if err := store.SaveMemory(context.Background(), "worker-b/agent", "k", "b-value"); err != nil {
			t.Fatalf("SaveMemory returned error: %v", err)
		}
		_, ok, err := store.Memory(context.Background(), "worker-a/agent", "k")
		if err != nil || !ok {
			t.Fatalf("Memory scope a ok=%v err=%v", ok, err)
		}
		listA, err := store.ListMemory(context.Background(), "worker-a/agent")
		if err != nil {
			t.Fatalf("ListMemory returned error: %v", err)
		}
		if len(listA) != 1 || listA[0].Value != "a-value" {
			t.Fatalf("ListMemory scope a = %+v, want single a-value", listA)
		}
	})

	t.Run("ListIsSortedByKey", func(t *testing.T) {
		store := newStore(t)
		for _, k := range []string{"gamma", "alpha", "beta"} {
			if err := store.SaveMemory(context.Background(), "s", k, "v"); err != nil {
				t.Fatalf("SaveMemory returned error: %v", err)
			}
		}
		entries, err := store.ListMemory(context.Background(), "s")
		if err != nil {
			t.Fatalf("ListMemory returned error: %v", err)
		}
		got := make([]string, len(entries))
		for i, e := range entries {
			got[i] = e.Key
		}
		want := []string{"alpha", "beta", "gamma"}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("ListMemory keys = %v, want %v", got, want)
			}
		}
	})

	t.Run("RedactsSecretsBeforePersisting", func(t *testing.T) {
		store := newStore(t)
		secret := "token=sk-abcdefghijklmnopqrstuvwxyz0123456789"
		if err := store.SaveMemory(context.Background(), "s", "k", secret); err != nil {
			t.Fatalf("SaveMemory returned error: %v", err)
		}
		value, ok, err := store.Memory(context.Background(), "s", "k")
		if err != nil || !ok {
			t.Fatalf("Memory ok=%v err=%v", ok, err)
		}
		if strings.Contains(value, "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Fatalf("Memory value leaked secret: %q", value)
		}
	})

	t.Run("RejectsOversizedValue", func(t *testing.T) {
		store := newStore(t)
		big := strings.Repeat("x", MaxMemoryValueBytes+1)
		if err := store.SaveMemory(context.Background(), "s", "k", big); err == nil {
			t.Fatal("SaveMemory accepted oversized value, want error")
		}
	})

	t.Run("RequiresScopeAndKey", func(t *testing.T) {
		store := newStore(t)
		if err := store.SaveMemory(context.Background(), "", "k", "v"); err == nil {
			t.Fatal("SaveMemory accepted empty scope, want error")
		}
		if err := store.SaveMemory(context.Background(), "s", "", "v"); err == nil {
			t.Fatal("SaveMemory accepted empty key, want error")
		}
	})

	t.Run("HonorsCanceledContext", func(t *testing.T) {
		store := newStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.SaveMemory(ctx, "s", "k", "v"); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveMemory error = %v, want context.Canceled", err)
		}
	})
}

func TestMemoryStoreMemoryConformance(t *testing.T) {
	assertMemoryConformance(t, func(t *testing.T) Store {
		return NewMemoryStore()
	})
}

func TestSQLiteStoreMemoryConformance(t *testing.T) {
	assertMemoryConformance(t, func(t *testing.T) Store {
		return newTestSQLiteStore(t, t.TempDir()+"/state.db")
	})
}

func TestSQLiteStoreMemoryPersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestSQLiteStore(t, path)
	if err := store.SaveMemory(context.Background(), "worker/agent", "fact", "deployed v2 on tuesday"); err != nil {
		t.Fatalf("SaveMemory returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	value, ok, err := reopened.Memory(context.Background(), "worker/agent", "fact")
	if err != nil {
		t.Fatalf("Memory returned error: %v", err)
	}
	if !ok || value != "deployed v2 on tuesday" {
		t.Fatalf("Memory = %q, ok=%v after reopen; want persisted value", value, ok)
	}
}
