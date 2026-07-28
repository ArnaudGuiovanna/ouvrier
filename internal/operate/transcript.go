package operate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TranscriptKind identifies one user-visible block in an operate session.
type TranscriptKind string

const (
	TranscriptUser       TranscriptKind = "user"
	TranscriptAssistant  TranscriptKind = "assistant"
	TranscriptToolCall   TranscriptKind = "tool_call"
	TranscriptToolResult TranscriptKind = "tool_result"
	TranscriptStatus     TranscriptKind = "status"
	TranscriptError      TranscriptKind = "error"
)

// TranscriptEntry is the append-only, replayable cockpit transcript. TUI,
// print, JSON, and RPC modes all render this same event source.
type TranscriptEntry struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	At        time.Time      `json:"at"`
	Kind      TranscriptKind `json:"kind"`
	Role      string         `json:"role,omitempty"`
	Text      string         `json:"text,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	Output    map[string]any `json:"output,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TranscriptStore appends and reads the session transcript.
type TranscriptStore struct {
	path     string
	redactor Redactor
	mu       sync.Mutex
}

func NewTranscriptStore(path string, redactor Redactor) *TranscriptStore {
	return &TranscriptStore{path: path, redactor: redactor}
}

func (s *TranscriptStore) Append(entry TranscriptEntry) (TranscriptEntry, error) {
	if s == nil || s.path == "" {
		return TranscriptEntry{}, errors.New("operate: transcript path is empty")
	}
	if entry.ID == "" {
		id, err := randomID()
		if err != nil {
			return TranscriptEntry{}, err
		}
		entry.ID = id
	}
	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	entry.Text = s.redactor.Redact(entry.Text)
	entry.Input = redactMap(s.redactor, entry.Input)
	entry.Output = redactMap(s.redactor, entry.Output)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return TranscriptEntry{}, fmt.Errorf("operate: create transcript dir: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return TranscriptEntry{}, fmt.Errorf("operate: open transcript: %w", err)
	}
	defer f.Close()
	data, err := json.Marshal(entry)
	if err != nil {
		return TranscriptEntry{}, fmt.Errorf("operate: encode transcript entry: %w", err)
	}
	if _, err := fmt.Fprintln(f, string(data)); err != nil {
		return TranscriptEntry{}, fmt.Errorf("operate: append transcript entry: %w", err)
	}
	if err := f.Sync(); err != nil {
		return TranscriptEntry{}, fmt.Errorf("operate: sync transcript entry: %w", err)
	}
	return entry, nil
}

func ReadTranscript(path string) ([]TranscriptEntry, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("operate: open transcript: %w", err)
	}
	defer f.Close()
	var entries []TranscriptEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var entry TranscriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("operate: parse transcript %s at line %d: %w", path, line, err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("operate: read transcript: %w", err)
	}
	return entries, nil
}

type jsonlTailRepair uint8

const (
	jsonlTailUnchanged jsonlTailRepair = iota
	jsonlTailCompleted
	jsonlTailDiscarded
)

// repairJSONLTail makes an append-only JSONL file safe for its next append. It
// completes a valid final record with a newline, discards only an invalid final
// fragment without a newline, and leaves middle corruption for the reader to
// reject explicitly.
func repairJSONLTail(path, label string) (jsonlTailRepair, error) {
	if path == "" {
		return jsonlTailUnchanged, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return jsonlTailUnchanged, nil
		}
		return jsonlTailUnchanged, fmt.Errorf("operate: open %s for recovery: %w", label, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return jsonlTailUnchanged, fmt.Errorf("operate: stat %s for recovery: %w", label, err)
	}
	size := info.Size()
	if size == 0 {
		return jsonlTailUnchanged, nil
	}
	last := []byte{0}
	if _, err := f.ReadAt(last, size-1); err != nil {
		return jsonlTailUnchanged, fmt.Errorf("operate: inspect %s tail: %w", label, err)
	}
	if last[0] == '\n' {
		return jsonlTailUnchanged, nil
	}

	const blockSize = int64(64 * 1024)
	start := size
	lineStart := int64(0)
	for start > 0 {
		end := start
		start -= blockSize
		if start < 0 {
			start = 0
		}
		block := make([]byte, end-start)
		if _, err := f.ReadAt(block, start); err != nil {
			return jsonlTailUnchanged, fmt.Errorf("operate: inspect %s tail: %w", label, err)
		}
		if index := bytes.LastIndexByte(block, '\n'); index >= 0 {
			lineStart = start + int64(index) + 1
			break
		}
	}
	const maxJSONLLine = int64(4 * 1024 * 1024)
	if size-lineStart > maxJSONLLine {
		return jsonlTailUnchanged, fmt.Errorf("operate: %s final line exceeds %d bytes", label, maxJSONLLine)
	}
	tail := make([]byte, size-lineStart)
	if _, err := f.ReadAt(tail, lineStart); err != nil {
		return jsonlTailUnchanged, fmt.Errorf("operate: read %s tail: %w", label, err)
	}
	if json.Valid(tail) {
		if _, err := f.WriteAt([]byte{'\n'}, size); err != nil {
			return jsonlTailUnchanged, fmt.Errorf("operate: complete %s final line: %w", label, err)
		}
		if err := f.Sync(); err != nil {
			return jsonlTailUnchanged, fmt.Errorf("operate: sync completed %s final line: %w", label, err)
		}
		return jsonlTailCompleted, nil
	}
	if err := f.Truncate(lineStart); err != nil {
		return jsonlTailUnchanged, fmt.Errorf("operate: truncate torn %s tail: %w", label, err)
	}
	if err := f.Sync(); err != nil {
		return jsonlTailUnchanged, fmt.Errorf("operate: sync repaired %s: %w", label, err)
	}
	return jsonlTailDiscarded, nil
}

func repairTrailingTranscript(path string) (bool, error) {
	repair, err := repairJSONLTail(path, "transcript")
	return repair == jsonlTailDiscarded, err
}

func redactMap(redactor Redactor, in map[string]any) map[string]any {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = redactValue(redactor, v)
	}
	return out
}

func redactValue(redactor Redactor, value any) any {
	switch typed := value.(type) {
	case string:
		return redactor.Redact(typed)
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			out[i] = redactor.Redact(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactValue(redactor, item)
		}
		return out
	case map[string]any:
		return redactMap(redactor, typed)
	default:
		return value
	}
}
