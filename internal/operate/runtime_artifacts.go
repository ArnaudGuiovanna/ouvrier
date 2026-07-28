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

func appendToolCall(path string, redactor Redactor, call plannedTool, result ToolResult, runErr error) error {
	if path == "" {
		return nil
	}
	record := map[string]any{
		"at":           time.Now().UTC(),
		"tool_call_id": call.ID,
		"tool":         call.Name,
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(f, string(data)); err != nil {
		return err
	}
	return f.Sync()
}

func readEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var events []Event
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, scanner.Err()
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
