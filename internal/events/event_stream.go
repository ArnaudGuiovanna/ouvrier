package events

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	mu     sync.RWMutex
	nextID uint64
	now    func() time.Time
	events []Event
}

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
				clone[childKey] = RedactJSONText(childValue)
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
		return value
	}
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
