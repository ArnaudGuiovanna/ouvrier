package events

import (
	"context"
	"errors"
)

// AppendPersisted serializes durable ID allocation with insertion into this
// in-process stream. The persist callback must atomically store the sanitized
// event and return it with a non-zero, globally allocated ID. Keeping that
// callback under the stream's append gate prevents concurrent local emitters
// from notifying subscribers out of durable-ID order, while the backing store
// remains the authority that makes IDs collision-free across replicas.
func (s *EventStream) AppendPersisted(ctx context.Context, event Event, persist func(context.Context, Event) (Event, error)) (Event, error) {
	if persist == nil {
		return Event{}, errors.New("event persistence callback is required")
	}
	if event.ID != 0 {
		return Event{}, errors.New("event ID must be allocated by durable persistence")
	}
	return s.append(ctx, event, persist)
}
