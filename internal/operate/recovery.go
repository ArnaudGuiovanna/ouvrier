package operate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

var ErrToolInterrupted = errors.New("operate: tool call interrupted before a durable result")

type pendingCall struct {
	id    string
	tool  string
	input map[string]any
}

// recoverInterruptedCalls closes calls that were made durable before a crash
// but have no durable result. It never executes the tool. The synthetic result
// is appended once, so subsequent resumes are idempotent and provider history
// remains structurally valid.
func (r *AgentRuntime) recoverInterruptedCalls(session *Session, entries []TranscriptEntry) ([]TranscriptEntry, error) {
	pending := make(map[string]pendingCall)
	calls := make(map[string]pendingCall)
	var order []string
	for _, entry := range entries {
		id := metaString(entry.Metadata, "tool_call_id")
		if id == "" {
			continue
		}
		switch entry.Kind {
		case TranscriptToolCall:
			if _, exists := pending[id]; !exists {
				order = append(order, id)
			}
			call := pendingCall{id: id, tool: entry.ToolName, input: entry.Input}
			pending[id] = call
			calls[id] = call
		case TranscriptToolResult:
			delete(pending, id)
		}
	}
	for _, id := range order {
		call, ok := pending[id]
		if !ok {
			continue
		}
		message := fmt.Sprintf("tool %s was interrupted; explicit operator action is required to retry", call.tool)
		entry, err := r.appendTranscript(session, TranscriptEntry{
			SessionID: session.ID,
			Kind:      TranscriptToolResult,
			ToolName:  call.tool,
			Output: map[string]any{
				"summary":     message,
				"error":       ErrToolInterrupted.Error(),
				"interrupted": true,
			},
			Metadata: map[string]any{
				"tool_call_id": call.id,
				"recovered":    true,
			},
		})
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := r.ensureInterruptedAudits(session, entries, calls); err != nil {
		return nil, err
	}
	return entries, nil
}

// ensureInterruptedAudits repairs the second half of recovery if a process
// dies after the synthetic transcript result is synced but before its audit
// line is synced. Existing audit IDs are never appended twice.
func (r *AgentRuntime) ensureInterruptedAudits(session *Session, entries []TranscriptEntry, calls map[string]pendingCall) error {
	audited, err := readToolCallAuditIDs(session.ToolCallsPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Kind != TranscriptToolResult || entry.Metadata["recovered"] != true {
			continue
		}
		id := metaString(entry.Metadata, "tool_call_id")
		if id == "" || audited[id] {
			continue
		}
		call, ok := calls[id]
		if !ok {
			return fmt.Errorf("operate: recovered tool result %s has no matching call", id)
		}
		summary, _ := entry.Output["summary"].(string)
		if err := r.appendToolCall(
			session,
			plannedTool{ID: id, Name: call.tool, Input: call.input},
			ToolResult{Summary: summary},
			ErrToolInterrupted,
		); err != nil {
			return fmt.Errorf("operate: persist interrupted tool-call audit: %w", err)
		}
		audited[id] = true
	}
	return nil
}

func readToolCallAuditIDs(path string) (map[string]bool, error) {
	ids := make(map[string]bool)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ids, nil
	}
	if err != nil {
		return nil, fmt.Errorf("operate: open tool-call audit: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("operate: parse tool-call audit %s at line %d: %w", path, line, err)
		}
		if id, _ := record["tool_call_id"].(string); id != "" {
			ids[id] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("operate: read tool-call audit: %w", err)
	}
	return ids, nil
}

func repairTrailingToolCallAudit(path string) (bool, error) {
	repair, err := repairJSONLTail(path, "tool-call audit")
	return repair == jsonlTailDiscarded, err
}
