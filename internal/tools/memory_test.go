package tools

import (
	"context"
	"sync"
	"testing"
)

type fakeMemoryStore struct {
	mu      sync.Mutex
	entries map[string]map[string]string
}

func newFakeMemoryStore() *fakeMemoryStore {
	return &fakeMemoryStore{entries: make(map[string]map[string]string)}
}

func (f *fakeMemoryStore) SaveMemory(_ context.Context, scope, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.entries[scope] == nil {
		f.entries[scope] = make(map[string]string)
	}
	f.entries[scope][key] = value
	return nil
}

func (f *fakeMemoryStore) Memory(_ context.Context, scope, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.entries[scope][key]
	return v, ok, nil
}

func (f *fakeMemoryStore) ListMemory(_ context.Context, scope string) ([]MemoryRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	records := make([]MemoryRecord, 0, len(f.entries[scope]))
	for k, v := range f.entries[scope] {
		records = append(records, MemoryRecord{Scope: scope, Key: k, Value: v})
	}
	return records, nil
}

func TestMemoryHelpersUseScopedStoreFromContext(t *testing.T) {
	store := newFakeMemoryStore()
	ctx := ContextWithMemoryStore(context.Background(), store, "worker/agent")

	if err := SaveMemory(ctx, "fact", "released v2"); err != nil {
		t.Fatalf("SaveMemory returned error: %v", err)
	}
	value, ok, err := Memory(ctx, "fact")
	if err != nil || !ok {
		t.Fatalf("Memory ok=%v err=%v", ok, err)
	}
	if value != "released v2" {
		t.Fatalf("Memory = %q, want %q", value, "released v2")
	}
	if store.entries["worker/agent"]["fact"] != "released v2" {
		t.Fatalf("store not written under scope: %+v", store.entries)
	}
}

func TestMemoryHelpersErrorWithoutStore(t *testing.T) {
	if err := SaveMemory(context.Background(), "k", "v"); err == nil {
		t.Fatal("SaveMemory without store returned nil, want error")
	}
	if _, _, err := Memory(context.Background(), "k"); err == nil {
		t.Fatal("Memory without store returned nil, want error")
	}
	if _, err := ListMemory(context.Background()); err == nil {
		t.Fatal("ListMemory without store returned nil, want error")
	}
}

func TestContextWithMemoryStoreIgnoresBlankScope(t *testing.T) {
	store := newFakeMemoryStore()
	ctx := ContextWithMemoryStore(context.Background(), store, "   ")
	if _, _, err := Memory(ctx, "k"); err == nil {
		t.Fatal("blank scope should leave ctx without a memory store")
	}
}
