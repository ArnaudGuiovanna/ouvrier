package events

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Event struct {
	ID        uint64
	At        time.Time
	Kind      EventKind
	ExecID    string
	SessionID string
	TraceID   string
	Payload   map[string]any
}

type EventStream struct {
	mu     sync.RWMutex
	nextID uint64
	now    func() time.Time
	events []Event
}

type Option func(*config) error

type config struct {
	now func() time.Time
}

func NewEventStream(opts ...Option) (*EventStream, error) {
	cfg := config{now: time.Now}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("nil event stream option")
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	return &EventStream{now: cfg.now}, nil
}

func WithClock(now func() time.Time) Option {
	return func(cfg *config) error {
		if now == nil {
			return errors.New("event stream clock is required")
		}
		cfg.now = now
		return nil
	}
}

func (s *EventStream) Append(ctx context.Context, event Event) (Event, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	event.ID = s.nextID
	event.Kind = CanonicalKind(event.Kind)
	if event.At.IsZero() {
		event.At = s.now().UTC()
	}
	event.Payload = clonePayload(event.Payload)
	s.events = append(s.events, event)
	return cloneEvent(event), nil
}

func (s *EventStream) List() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]Event, len(s.events))
	for i, event := range s.events {
		events[i] = cloneEvent(event)
	}
	return events
}

func (s *EventStream) Since(id uint64) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]Event, 0, len(s.events))
	for _, event := range s.events {
		if event.ID > id {
			events = append(events, cloneEvent(event))
		}
	}
	return events
}

func cloneEvent(event Event) Event {
	event.Payload = clonePayload(event.Payload)
	return event
}

func clonePayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	clone := make(map[string]any, len(payload))
	for key, value := range payload {
		clone[key] = value
	}
	return clone
}
