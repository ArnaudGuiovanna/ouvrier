package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Hook func(context.Context, Event) (Event, error)

type HookBus struct {
	mu       sync.RWMutex
	handlers map[EventKind][]Hook
}

func NewHookBus() *HookBus {
	return &HookBus{handlers: make(map[EventKind][]Hook)}
}

func (b *HookBus) Register(kind EventKind, hook Hook) error {
	if b == nil {
		return errors.New("hook bus is required")
	}
	if kind == "" {
		return errors.New("hook event kind is required")
	}
	if hook == nil {
		return errors.New("hook is required")
	}

	kind = CanonicalKind(kind)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[kind] = append(b.handlers[kind], hook)
	return nil
}

func (b *HookBus) Emit(ctx context.Context, event Event) (Event, error) {
	if b == nil {
		return event, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}

	event.Kind = CanonicalKind(event.Kind)
	b.mu.RLock()
	hooks := append([]Hook(nil), b.handlers[event.Kind]...)
	b.mu.RUnlock()

	next := cloneEvent(event)
	for _, hook := range hooks {
		updated, err := hook(ctx, cloneEvent(next))
		if err != nil {
			return Event{}, fmt.Errorf("hook %s blocked event: %w", event.Kind, err)
		}
		next = cloneEvent(updated)
	}
	return next, nil
}
