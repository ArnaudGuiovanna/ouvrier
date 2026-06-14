package operate

import "testing"

func TestStoreLatestSessionID(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	first, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	// Touch the second so it is unambiguously the most recent.
	if err := store.Save(second); err != nil {
		t.Fatalf("save second: %v", err)
	}

	latest, err := store.LatestSessionID()
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest != second.ID {
		t.Fatalf("latest = %q, want %q (first=%q)", latest, second.ID, first.ID)
	}
}

func TestStoreLatestSessionIDEmpty(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.LatestSessionID(); err == nil {
		t.Fatal("expected an error when no sessions exist")
	}
}
