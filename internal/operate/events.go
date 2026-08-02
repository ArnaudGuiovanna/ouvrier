package operate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	maxEventJSONLLineBytes  = 4 * 1024 * 1024
	maxEventReplayFileBytes = 64 * 1024 * 1024
	maxEventReplayEntries   = 100_000
)

// EventLog is an append-only JSONL stream for one operate session.
type EventLog struct {
	path     string
	redactor Redactor
	mu       sync.Mutex
}

// NewEventLog creates an event stream at path.
func NewEventLog(path string, redactor Redactor) *EventLog {
	return &EventLog{path: path, redactor: redactor}
}

// Event appends a normalized event. It satisfies EventSink. Persistence errors
// are returned so an agent turn cannot report success after losing its audit
// trail.
func (l *EventLog) Event(event Event) error {
	if l == nil || l.path == "" {
		return nil
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Message = l.redactor.Redact(event.Message)
	event.Command = l.redactor.Redact(event.Command)
	event.Path = l.redactor.Redact(event.Path)
	event.Metadata = redactMap(l.redactor, event.Metadata)
	data, err := json.Marshal(event)
	if err != nil {
		return redactError(l.redactor, fmt.Errorf("operate: encode event: %w", err))
	}
	if len(data) > maxEventJSONLLineBytes {
		return redactError(l.redactor, fmt.Errorf("operate: event record exceeds %d bytes", maxEventJSONLLineBytes))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := openSessionArtifact(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600, true)
	if err != nil {
		return redactError(l.redactor, fmt.Errorf("operate: open event log: %w", err))
	}
	if _, err := fmt.Fprintln(f, string(data)); err != nil {
		_ = f.Close()
		return redactError(l.redactor, fmt.Errorf("operate: append event: %w", err))
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return redactError(l.redactor, fmt.Errorf("operate: sync event: %w", err))
	}
	if err := f.Close(); err != nil {
		return redactError(l.redactor, fmt.Errorf("operate: close event log: %w", err))
	}
	return nil
}

// MultiSink forwards events to every non-nil sink.
type MultiSink []EventSink

// Event forwards event to every sink.
func (s MultiSink) Event(event Event) error {
	var errs []error
	for _, sink := range s {
		if sink != nil {
			errs = append(errs, sink.Event(event))
		}
	}
	return errors.Join(errs...)
}
