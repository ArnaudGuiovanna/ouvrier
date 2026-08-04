package acp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func (c *client) handleNotification(message wireMessage) error {
	if message.Method != "session/update" {
		return nil
	}
	var envelope struct {
		Update json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(message.Params, &envelope); err != nil {
		return fmt.Errorf("decode session/update: %w", err)
	}
	var header struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(envelope.Update, &header); err != nil {
		return fmt.Errorf("decode session/update kind: %w", err)
	}
	switch header.SessionUpdate {
	case "agent_message_chunk":
		var update struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(envelope.Update, &update); err != nil {
			return fmt.Errorf("decode agent message chunk: %w", err)
		}
		text := update.Content.Text
		if len(text) > maxAgentTextBytes-c.textBytes {
			return fmt.Errorf("agent text exceeds %d bytes", maxAgentTextBytes)
		}
		c.textBytes += len(text)
		safe := c.textRedactor.Push(text)
		if safe == "" {
			return nil
		}
		if len(safe) > maxAgentTextBytes-c.text.Len() {
			return fmt.Errorf("redacted agent text exceeds %d bytes", maxAgentTextBytes)
		}
		c.text.WriteString(safe)
		return c.emit(operate.Event{At: time.Now().UTC(), Kind: operate.EventAgentDelta, Message: safe})
	case "tool_call", "tool_call_update":
		var update struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
			Status     string `json:"status"`
			Locations  []struct {
				Path string `json:"path"`
			} `json:"locations"`
		}
		if err := json.Unmarshal(envelope.Update, &update); err != nil {
			return fmt.Errorf("decode %s: %w", header.SessionUpdate, err)
		}
		safeID := c.req.Redactor.Redact(update.ToolCallID)
		if header.SessionUpdate == "tool_call" {
			title := c.req.Redactor.Redact(strings.TrimSpace(update.Title))
			c.tools[update.ToolCallID] = toolState{Title: title, Kind: strings.ToLower(update.Kind)}
			return c.emit(operate.Event{At: time.Now().UTC(), Kind: operate.EventCommandStarted, Command: title, Metadata: map[string]interface{}{
				"tool_call_id": safeID, "kind": c.req.Redactor.Redact(update.Kind), "transport": "acp/v1",
			}})
		}
		state := c.tools[update.ToolCallID]
		if update.Title != "" {
			state.Title = c.req.Redactor.Redact(update.Title)
		}
		if update.Kind != "" {
			state.Kind = strings.ToLower(update.Kind)
		}
		c.tools[update.ToolCallID] = state
		if update.Status != "completed" && update.Status != "failed" {
			return nil
		}
		exitCode := 0
		if update.Status == "failed" {
			exitCode = 1
		}
		if err := c.emit(operate.Event{At: time.Now().UTC(), Kind: operate.EventCommandFinished, Command: state.Title, ExitCode: exitCode, Metadata: map[string]interface{}{
			"tool_call_id": safeID, "kind": c.req.Redactor.Redact(state.Kind), "status": c.req.Redactor.Redact(update.Status), "transport": "acp/v1",
		}}); err != nil {
			return err
		}
		for _, location := range update.Locations {
			if safe := c.safeLocation(location.Path); safe != "" {
				if err := c.emit(operate.Event{At: time.Now().UTC(), Kind: operate.EventFileChanged, Path: safe, Metadata: map[string]interface{}{"tool_call_id": safeID}}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *client) flushText() error {
	if c == nil || c.textRedactor == nil {
		return nil
	}
	safe := c.textRedactor.Flush()
	if safe == "" {
		return nil
	}
	if len(safe) > maxAgentTextBytes-c.text.Len() {
		return fmt.Errorf("redacted agent text exceeds %d bytes", maxAgentTextBytes)
	}
	c.text.WriteString(safe)
	return c.emit(operate.Event{At: time.Now().UTC(), Kind: operate.EventAgentDelta, Message: safe})
}

func (c *client) safeLocation(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.req.CWD, path)
	}
	rel, err := filepath.Rel(c.req.CWD, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return c.req.Redactor.Redact(filepath.ToSlash(rel))
}
