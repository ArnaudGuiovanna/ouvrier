package events

import (
	"context"
	"errors"
	"strings"
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

const redactedPayloadValue = "[REDACTED]"

type Option func(*config) error

type config struct {
	now       func() time.Time
	initialID uint64
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
	return &EventStream{now: cfg.now, nextID: cfg.initialID}, nil
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

func WithInitialID(id uint64) Option {
	return func(cfg *config) error {
		cfg.initialID = id
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
	event.Payload = sanitizePayload(event.Payload)
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

func SanitizeEvent(event Event) Event {
	event.Kind = CanonicalKind(event.Kind)
	event.Payload = sanitizePayload(event.Payload)
	return event
}

func cloneEvent(event Event) Event {
	return SanitizeEvent(event)
}

func sanitizePayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	clone := make(map[string]any, len(payload))
	for key, value := range payload {
		clone[key] = sanitizePayloadValue(key, value)
	}
	return clone
}

func sanitizePayloadValue(key string, value any) any {
	if isSensitivePayloadKey(key) {
		return redactedPayloadValue
	}
	switch typed := value.(type) {
	case map[string]any:
		return sanitizePayload(typed)
	case map[string]string:
		clone := make(map[string]string, len(typed))
		for childKey, childValue := range typed {
			if isSensitivePayloadKey(childKey) {
				clone[childKey] = redactedPayloadValue
			} else {
				clone[childKey] = childValue
			}
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for i, item := range typed {
			clone[i] = sanitizePayloadValue("", item)
		}
		return clone
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

func isSensitivePayloadKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "authorization", "token", "api_key", "password", "secret", "cookie":
		return true
	}
	return strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_secret") ||
		strings.HasSuffix(normalized, "_password") ||
		strings.Contains(normalized, "api_key") ||
		strings.HasSuffix(normalized, "_cookie")
}
