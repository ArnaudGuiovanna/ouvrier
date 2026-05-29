package tools

import (
	"context"
	"errors"
	"strings"
)

// MemoryStore is the scoped persistent-memory contract threaded into tool
// handlers so pipes and subagents can read and write agent memory that
// survives across sessions. The harness supplies a concrete store; handlers
// reach it through the context helpers below.
type MemoryStore interface {
	SaveMemory(ctx context.Context, scope, key, value string) error
	Memory(ctx context.Context, scope, key string) (string, bool, error)
	ListMemory(ctx context.Context, scope string) ([]MemoryRecord, error)
}

// MemoryRecord mirrors a single persisted memory entry for tool handlers.
type MemoryRecord struct {
	Scope     string
	Key       string
	Value     string
	UpdatedAt int64
}

type memoryContext struct {
	store MemoryStore
	scope string
}

type memoryContextKey struct{}

// ContextWithMemoryStore threads a scoped memory store into ctx. scope must
// identify the worker plus logical agent so concurrent agents stay isolated.
// A nil store or blank scope leaves ctx unchanged.
func ContextWithMemoryStore(ctx context.Context, store MemoryStore, scope string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if store == nil || strings.TrimSpace(scope) == "" {
		return ctx
	}
	return context.WithValue(ctx, memoryContextKey{}, memoryContext{
		store: store,
		scope: strings.TrimSpace(scope),
	})
}

func memoryFromContext(ctx context.Context) (memoryContext, bool) {
	if ctx == nil {
		return memoryContext{}, false
	}
	mem, ok := ctx.Value(memoryContextKey{}).(memoryContext)
	return mem, ok && mem.store != nil && mem.scope != ""
}

// SaveMemory persists a value into the agent memory scoped to the current
// session. It returns an error when no memory store is configured.
func SaveMemory(ctx context.Context, key, value string) error {
	mem, ok := memoryFromContext(ctx)
	if !ok {
		return errors.New("agent memory is not configured for this execution")
	}
	return mem.store.SaveMemory(ctx, mem.scope, key, value)
}

// Memory reads a value from the agent memory scoped to the current session.
func Memory(ctx context.Context, key string) (string, bool, error) {
	mem, ok := memoryFromContext(ctx)
	if !ok {
		return "", false, errors.New("agent memory is not configured for this execution")
	}
	return mem.store.Memory(ctx, mem.scope, key)
}

// ListMemory lists all entries in the agent memory scoped to the current session.
func ListMemory(ctx context.Context) ([]MemoryRecord, error) {
	mem, ok := memoryFromContext(ctx)
	if !ok {
		return nil, errors.New("agent memory is not configured for this execution")
	}
	return mem.store.ListMemory(ctx, mem.scope)
}
