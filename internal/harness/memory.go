package harness

import (
	"context"
	"strings"

	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// memoryScopeFor returns the stable scope for persistent agent memory. When a
// scope is configured it is used verbatim so a logical agent's memory persists
// across sessions; otherwise the model acts as a deterministic fallback so the
// same logical agent reuses its memory without bleeding into other models.
func (h *Harness) memoryScopeFor(session runtimecore.Session) string {
	if scope := strings.TrimSpace(h.memoryScope); scope != "" {
		return scope
	}
	if model := strings.TrimSpace(session.Model); model != "" {
		return "model:" + model
	}
	return "model:" + strings.TrimSpace(h.model)
}

// memoryStoreAdapter bridges the durable state.Store to the tools.MemoryStore
// contract threaded into tool handlers.
type memoryStoreAdapter struct {
	store state.Store
}

func (a memoryStoreAdapter) SaveMemory(ctx context.Context, scope, key, value string) error {
	return a.store.SaveMemory(ctx, scope, key, value)
}

func (a memoryStoreAdapter) Memory(ctx context.Context, scope, key string) (string, bool, error) {
	return a.store.Memory(ctx, scope, key)
}

func (a memoryStoreAdapter) ListMemory(ctx context.Context, scope string) ([]tools.MemoryRecord, error) {
	records, err := a.store.ListMemory(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make([]tools.MemoryRecord, len(records))
	for i, r := range records {
		out[i] = tools.MemoryRecord{
			Scope:     r.Scope,
			Key:       r.Key,
			Value:     r.Value,
			UpdatedAt: r.UpdatedAt.UnixNano(),
		}
	}
	return out, nil
}
