package operate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	runtimepkg "runtime"
	"strings"
	"time"
)

func (r *AgentRuntime) appendToolCall(session *Session, call plannedTool, result ToolResult, runErr error) error {
	if err := r.requireSessionWriter(session); err != nil {
		return err
	}
	return appendToolCall(session.ToolCallsPath, r.Options.Redactor, call, result, runErr)
}

func appendToolCall(path string, redactor Redactor, call plannedTool, result ToolResult, runErr error) error {
	if path == "" {
		return nil
	}
	record := map[string]any{
		"at":           time.Now().UTC(),
		"tool_call_id": redactor.Redact(call.ID),
		"tool":         redactor.Redact(call.Name),
		"input":        redactMap(redactor, call.Input),
		"summary":      redactor.Redact(result.Summary),
		"data":         redactMap(redactor, result.Data),
	}
	if runErr != nil {
		record["error"] = redactor.Redact(runErr.Error())
	}
	return appendJSONLine(path, record)
}

func appendJSONLine(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := validateJSONLLineSize(data, "JSONL record"); err != nil {
		return err
	}
	f, err := openSessionArtifact(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600, true)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(f, string(data)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readEvents(path string) ([]Event, error) {
	f, err := openSessionArtifact(path, os.O_RDONLY, 0, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("operate: inspect events %s: %w", path, err)
	}
	if info.Size() > maxEventReplayFileBytes {
		return nil, fmt.Errorf("operate: events %s exceeds %d bytes", path, maxEventReplayFileBytes)
	}
	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventJSONLLineBytes+1)
	line := 0
	for scanner.Scan() {
		line++
		if line > maxEventReplayEntries {
			return nil, fmt.Errorf("operate: events %s exceeds %d entries", path, maxEventReplayEntries)
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("operate: parse events %s at line %d: %w", path, line, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("operate: read events %s at line %d: %w", path, line+1, err)
	}
	return events, nil
}

func repairTrailingEvents(path string) (bool, error) {
	repair, err := repairJSONLTail(path, "event journal")
	return repair == jsonlTailDiscarded, err
}

func detectOperateRepoRoot() string {
	_, file, _, ok := runtimepkg.Caller(0)
	if !ok {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	dir := filepath.Dir(file)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/ArnaudGuiovanna/ouvrier") {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
		dir = next
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
