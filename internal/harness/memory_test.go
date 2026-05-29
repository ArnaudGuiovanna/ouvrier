package harness

import (
	"context"
	"testing"

	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestMemoryScopeForPrefersConfiguredScope(t *testing.T) {
	h := &Harness{memoryScope: "billing-worker/triage", model: "anthropic/claude-sonnet-4-6"}
	got := h.memoryScopeFor(runtimecore.Session{Model: "openai/gpt-5.1"})
	if got != "billing-worker/triage" {
		t.Fatalf("memoryScopeFor = %q, want configured scope", got)
	}
}

func TestMemoryScopeForFallsBackToModel(t *testing.T) {
	h := &Harness{model: "anthropic/claude-sonnet-4-6"}
	got := h.memoryScopeFor(runtimecore.Session{Model: "openai/gpt-5.1"})
	if got != "model:openai/gpt-5.1" {
		t.Fatalf("memoryScopeFor = %q, want session model fallback", got)
	}
}

func TestMemoryStoreAdapterPersistsAndConverts(t *testing.T) {
	adapter := memoryStoreAdapter{store: state.NewMemoryStore()}
	if err := adapter.SaveMemory(context.Background(), "s", "k", "v"); err != nil {
		t.Fatalf("SaveMemory returned error: %v", err)
	}
	value, ok, err := adapter.Memory(context.Background(), "s", "k")
	if err != nil || !ok || value != "v" {
		t.Fatalf("Memory = %q ok=%v err=%v", value, ok, err)
	}
	records, err := adapter.ListMemory(context.Background(), "s")
	if err != nil {
		t.Fatalf("ListMemory returned error: %v", err)
	}
	if len(records) != 1 || records[0].Key != "k" || records[0].UpdatedAt == 0 {
		t.Fatalf("ListMemory = %+v, want one record with timestamp", records)
	}
}
