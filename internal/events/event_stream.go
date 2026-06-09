package events

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
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
	mu          sync.RWMutex
	nextID      uint64
	now         func() time.Time
	events      []Event
	subscribers []Subscriber
}

// Subscriber is called for every Event appended to the stream, after
// sanitization and ID assignment. Subscribers must not mutate the supplied
// Event and must return quickly; long-running observers should hand the event
// off to their own goroutine. Subscribers run synchronously under the stream
// lock; panics are recovered to keep the runtime from crashing on a broken
// observer.
type Subscriber func(context.Context, Event)

const redactedPayloadValue = "[REDACTED]"

var (
	authorizationPattern       = regexp.MustCompile(`(?i)\bauthorization(\s*[:=]\s*)(?:bearer\s+)?[^,\s"'}]+`)
	sensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b((?:access|refresh|session)?[-_]?token|api[-_]?key|password|secret(?:[-_]?key)?|client[-_]?secret|private[-_]?key|cookie)(\s*[:=]\s*)([^,\s"'}]+)`)
	bearerTokenPattern         = regexp.MustCompile(`(?i)\bBearer\s+[^,\s"'}]+`)
)

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
	s.nextID++
	event.ID = s.nextID
	event.Kind = CanonicalKind(event.Kind)
	if event.At.IsZero() {
		event.At = s.now().UTC()
	}
	event.Payload = sanitizePayload(event.Payload)
	s.events = append(s.events, event)
	subscribers := append([]Subscriber(nil), s.subscribers...)
	stored := cloneEvent(event)
	s.mu.Unlock()

	for _, sub := range subscribers {
		if sub == nil {
			continue
		}
		notifySubscriber(ctx, sub, stored)
	}
	return stored, nil
}

// EnsureNextIDAtLeast advances the stream counter to id when external durable
// storage already contains events up to that identifier.
func (s *EventStream) EnsureNextIDAtLeast(id uint64) {
	if s == nil || id == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nextID < id {
		s.nextID = id
	}
}

// Subscribe registers a Subscriber. Subscribers receive each Event after it is
// appended. Pass a nil Subscriber to no-op (returns an error). Subscriber
// registration is concurrency-safe.
func (s *EventStream) Subscribe(sub Subscriber) error {
	if s == nil {
		return errors.New("event stream is required")
	}
	if sub == nil {
		return errors.New("event subscriber is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, sub)
	return nil
}

func notifySubscriber(ctx context.Context, sub Subscriber, event Event) {
	defer func() {
		_ = recover()
	}()
	sub(ctx, event)
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
				clone[childKey] = RedactJSONText(childValue)
			}
		}
		return clone
	case map[string][]string:
		clone := make(map[string][]string, len(typed))
		for childKey, childValues := range typed {
			if isSensitivePayloadKey(childKey) {
				clone[childKey] = []string{redactedPayloadValue}
				continue
			}
			cloneValues := make([]string, len(childValues))
			for i, childValue := range childValues {
				cloneValues[i] = RedactJSONText(childValue)
			}
			clone[childKey] = cloneValues
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for i, item := range typed {
			clone[i] = sanitizePayloadValue("", item)
		}
		return clone
	case []string:
		clone := make([]string, len(typed))
		for i, item := range typed {
			clone[i] = RedactJSONText(item)
		}
		return clone
	case []byte:
		return []byte(RedactJSONText(string(typed)))
	case string:
		return RedactJSONText(typed)
	default:
		if sanitized, ok := sanitizeReflectPayloadValue(typed); ok {
			return sanitized
		}
		return value
	}
}

func sanitizeReflectPayloadValue(value any) (any, bool) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false
	}
	switch rv.Kind() {
	case reflect.Map:
		return sanitizeReflectMapPayloadValue(rv)
	case reflect.Slice, reflect.Array:
		return sanitizeReflectSlicePayloadValue(rv), true
	default:
		return nil, false
	}
}

func sanitizeReflectMapPayloadValue(rv reflect.Value) (any, bool) {
	if rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	switch rv.Type().Elem().Kind() {
	case reflect.String:
		clone := make(map[string]string, rv.Len())
		for _, key := range rv.MapKeys() {
			childKey := key.String()
			if isSensitivePayloadKey(childKey) {
				clone[childKey] = redactedPayloadValue
				continue
			}
			clone[childKey] = RedactJSONText(rv.MapIndex(key).String())
		}
		return clone, true
	case reflect.Slice:
		if rv.Type().Elem().Elem().Kind() == reflect.String {
			clone := make(map[string][]string, rv.Len())
			for _, key := range rv.MapKeys() {
				childKey := key.String()
				if isSensitivePayloadKey(childKey) {
					clone[childKey] = []string{redactedPayloadValue}
					continue
				}
				childValues := rv.MapIndex(key)
				cloneValues := make([]string, childValues.Len())
				for i := 0; i < childValues.Len(); i++ {
					cloneValues[i] = RedactJSONText(childValues.Index(i).String())
				}
				clone[childKey] = cloneValues
			}
			return clone, true
		}
	}

	clone := make(map[string]any, rv.Len())
	for _, key := range rv.MapKeys() {
		childKey := key.String()
		if isSensitivePayloadKey(childKey) {
			clone[childKey] = redactedPayloadValue
			continue
		}
		clone[childKey] = sanitizePayloadValue(childKey, rv.MapIndex(key).Interface())
	}
	return clone, true
}

func sanitizeReflectSlicePayloadValue(rv reflect.Value) []any {
	clone := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		item := rv.Index(i)
		if item.CanInterface() {
			clone[i] = sanitizePayloadValue("", item.Interface())
		}
	}
	return clone
}

func isSensitivePayloadKey(key string) bool {
	normalized := normalizeSensitivePayloadKey(key)
	switch normalized {
	case "authorization", "token", "api_key", "password", "secret", "cookie", "private_key", "secret_key", "client_secret":
		return true
	}
	return strings.HasSuffix(normalized, "_token") ||
		strings.HasSuffix(normalized, "_secret") ||
		strings.HasSuffix(normalized, "_password") ||
		strings.HasSuffix(normalized, "_key") ||
		strings.Contains(normalized, "api_key") ||
		strings.HasSuffix(normalized, "_cookie")
}

func normalizeSensitivePayloadKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	runes := []rune(key)
	var b strings.Builder
	b.Grow(len(key) + 4)
	var previousWasSeparator bool
	for i, r := range runes {
		switch {
		case r == '-' || r == '_' || r == '.' || r == ' ':
			if b.Len() > 0 && !previousWasSeparator {
				b.WriteByte('_')
				previousWasSeparator = true
			}
		case r >= 'A' && r <= 'Z':
			if shouldInsertCamelSeparator(runes, i, previousWasSeparator) {
				b.WriteByte('_')
			}
			b.WriteByte(byte(r + ('a' - 'A')))
			previousWasSeparator = false
		default:
			b.WriteRune(r)
			previousWasSeparator = false
		}
	}
	return strings.Trim(b.String(), "_")
}

func shouldInsertCamelSeparator(runes []rune, i int, previousWasSeparator bool) bool {
	if i == 0 || previousWasSeparator {
		return false
	}
	prev := runes[i-1]
	if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
		return true
	}
	if prev >= 'A' && prev <= 'Z' && i+1 < len(runes) {
		next := runes[i+1]
		return next >= 'a' && next <= 'z'
	}
	return false
}

func redactSensitiveString(value string) string {
	if value == "" {
		return value
	}
	redacted := authorizationPattern.ReplaceAllString(value, "Authorization${1}"+redactedPayloadValue)
	redacted = bearerTokenPattern.ReplaceAllString(redacted, "Bearer "+redactedPayloadValue)
	redacted = sensitiveAssignmentPattern.ReplaceAllString(redacted, "${1}${2}"+redactedPayloadValue)
	for _, secret := range sensitiveEnvironmentValues() {
		redacted = strings.ReplaceAll(redacted, secret, redactedPayloadValue)
	}
	return redacted
}

// RedactText removes common credential forms from free-form text.
func RedactText(value string) string {
	return redactSensitiveString(value)
}

// RedactJSONText redacts sensitive JSON keys recursively and credential-looking string values.
func RedactJSONText(value string) string {
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return RedactText(value)
	}
	redacted, changed := redactJSONValue("", decoded)
	if !changed {
		return value
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return RedactText(value)
	}
	return string(encoded)
}

func redactJSONValue(key string, value any) (any, bool) {
	if isSensitivePayloadKey(key) {
		return redactedPayloadValue, true
	}
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		changed := false
		for childKey, childValue := range typed {
			var childChanged bool
			clone[childKey], childChanged = redactJSONValue(childKey, childValue)
			changed = changed || childChanged
		}
		return clone, changed
	case []any:
		clone := make([]any, len(typed))
		changed := false
		for i, item := range typed {
			var itemChanged bool
			clone[i], itemChanged = redactJSONValue("", item)
			changed = changed || itemChanged
		}
		return clone, changed
	case string:
		redacted := RedactText(typed)
		return redacted, redacted != typed
	default:
		return value, false
	}
}

func sensitiveEnvironmentValues() []string {
	values := make([]string, 0)
	seen := make(map[string]struct{})
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || len(value) < 4 || !isSensitivePayloadKey(key) {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}
