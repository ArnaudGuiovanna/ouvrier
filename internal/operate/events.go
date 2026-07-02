package operate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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

// Event appends a normalized event. It satisfies EventSink.
func (l *EventLog) Event(event Event) {
	if l == nil || l.path == "" {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Message = l.redactor.Redact(event.Message)
	event.Command = l.redactor.Redact(event.Command)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, string(data))
}

// MultiSink forwards events to every non-nil sink.
type MultiSink []EventSink

// Event forwards event to every sink.
func (s MultiSink) Event(event Event) {
	for _, sink := range s {
		if sink != nil {
			sink.Event(event)
		}
	}
}
