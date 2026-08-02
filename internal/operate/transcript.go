package operate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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

// maxJSONLLineBytes is shared by every operate journal writer, reader, and
// tail-repair path. Eight MiB accommodates the advertised one-MiB worker-file
// input even under worst-case JSON escaping while still bounding allocations.
const maxJSONLLineBytes = 8 * 1024 * 1024

const (
	maxTranscriptFileBytes = 64 * 1024 * 1024
	maxTranscriptEntries   = 100_000
)

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
	entry = redactTranscriptEntry(s.redactor, entry)
	data, err := json.Marshal(entry)
	if err != nil {
		return TranscriptEntry{}, redactError(s.redactor, fmt.Errorf("operate: encode transcript entry: %w", err))
	}
	if err := validateJSONLLineSize(data, "transcript entry"); err != nil {
		return TranscriptEntry{}, redactError(s.redactor, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := openSessionArtifact(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600, true)
	if err != nil {
		return TranscriptEntry{}, redactError(s.redactor, fmt.Errorf("operate: open transcript: %w", err))
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return TranscriptEntry{}, redactError(s.redactor, fmt.Errorf("operate: stat transcript: %w", err))
	}
	if info.Size() > int64(maxTranscriptFileBytes-len(data)-1) {
		_ = f.Close()
		return TranscriptEntry{}, fmt.Errorf("operate: transcript exceeds %d bytes; fork or start a fresh session", maxTranscriptFileBytes)
	}
	if _, err := fmt.Fprintln(f, string(data)); err != nil {
		_ = f.Close()
		return TranscriptEntry{}, redactError(s.redactor, fmt.Errorf("operate: append transcript entry: %w", err))
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return TranscriptEntry{}, redactError(s.redactor, fmt.Errorf("operate: sync transcript entry: %w", err))
	}
	if err := f.Close(); err != nil {
		return TranscriptEntry{}, redactError(s.redactor, fmt.Errorf("operate: close transcript entry: %w", err))
	}
	return entry, nil
}

func ReadTranscript(path string) ([]TranscriptEntry, error) {
	if path == "" {
		return nil, nil
	}
	f, err := openSessionArtifact(path, os.O_RDONLY, 0, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("operate: open transcript: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("operate: stat transcript: %w", err)
	}
	if info.Size() > maxTranscriptFileBytes {
		return nil, fmt.Errorf("operate: transcript exceeds %d bytes", maxTranscriptFileBytes)
	}
	var entries []TranscriptEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJSONLLineBytes+1)
	line := 0
	for scanner.Scan() {
		line++
		if line > maxTranscriptEntries {
			return nil, fmt.Errorf("operate: transcript exceeds %d entries", maxTranscriptEntries)
		}
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
	f, err := openSessionArtifact(path, os.O_RDWR, 0, false)
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
	if size-lineStart > int64(maxJSONLLineBytes) {
		return jsonlTailUnchanged, fmt.Errorf("operate: %s final line exceeds %d bytes", label, maxJSONLLineBytes)
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

func validateJSONLLineSize(data []byte, label string) error {
	if len(data) > maxJSONLLineBytes {
		return fmt.Errorf("operate: %s exceeds %d bytes", label, maxJSONLLineBytes)
	}
	return nil
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
		key := redactor.Redact(k)
		if sensitiveDataKey(k) {
			out[key] = "***"
			continue
		}
		out[key] = redactValue(redactor, v)
	}
	return out
}

// sensitiveDataKey masks secrets even when their concrete value was not
// present in the process environment used to construct the Redactor. Avoid
// broad substring matching so harmless telemetry such as token_count remains
// observable.
func sensitiveDataKey(key string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(key))
	normalized = strings.NewReplacer("-", "_", ".", "_").Replace(normalized)
	if secretEnvironmentName(normalized) {
		return true
	}
	switch normalized {
	case "AUTHORIZATION", "PROXY_AUTHORIZATION", "COOKIE", "SET_COOKIE",
		"CLIENT_SECRET", "PRIVATE_KEY", "CREDENTIALS", "AUTH_TOKEN", "BEARER_TOKEN":
		return true
	default:
		return false
	}
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
	case map[string]string:
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			redactedKey := redactor.Redact(key)
			if sensitiveDataKey(key) {
				out[redactedKey] = "***"
				continue
			}
			out[redactedKey] = redactor.Redact(item)
		}
		return out
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(typed, &decoded) == nil {
			if encoded, err := json.Marshal(redactValue(redactor, decoded)); err == nil {
				return json.RawMessage(encoded)
			}
		}
		return json.RawMessage(redactor.Redact(string(typed)))
	case []byte:
		return []byte(redactor.Redact(string(typed)))
	case nil, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return value
	default:
		// Metadata can carry typed structs, pointers, aliases, and nested slices.
		// Normalize any JSON-compatible value and recurse so those shapes cannot
		// bypass redaction merely because their concrete Go type is not map/[]any.
		encoded, err := json.Marshal(value)
		if err != nil {
			return value
		}
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return value
		}
		return redactValue(redactor, decoded)
	}
}
