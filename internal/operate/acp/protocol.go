package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

type client struct {
	writer       io.Writer
	scan         *bufio.Scanner
	req          operate.TurnRequest
	sink         operate.EventSink
	nextID       int64
	rawBytes     int
	textBytes    int
	text         strings.Builder
	textRedactor *operate.RedactionStream
	tools        map[string]toolState
}

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type toolState struct {
	Title string
	Kind  string
}

// ErrAuthenticationRequired marks an ACP agent whose persisted login is
// missing, expired, or rejected by the upstream model service.
var ErrAuthenticationRequired = errors.New("ACP agent authentication required")

func newClient(writer io.Writer, reader io.Reader, req operate.TurnRequest, sink operate.EventSink) *client {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxProtocolLineBytes)
	return &client{
		writer: writer, scan: scanner, req: req, sink: sink,
		textRedactor: req.Redactor.NewStream(), tools: make(map[string]toolState),
	}
}

func (c *client) run(ctx context.Context) (runErr error) {
	defer func() {
		runErr = errors.Join(runErr, c.flushText())
	}()
	var initialized struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", map[string]interface{}{
		"protocolVersion": 1,
		"clientCapabilities": map[string]interface{}{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "ouvrier", "title": "Ouvrier", "version": "1"},
	}, &initialized); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if initialized.ProtocolVersion != 1 {
		return fmt.Errorf("unsupported negotiated protocol version %d (want 1)", initialized.ProtocolVersion)
	}

	var session sessionResponse
	if err := c.call(ctx, "session/new", governedSessionParams(c.req), &session); err != nil {
		return fmt.Errorf("create session using the selected agent's saved local session: %w", err)
	}
	if err := c.enforceGovernedMode(ctx, session); err != nil {
		return err
	}

	prompt, err := promptWithContext(c.req)
	if err != nil {
		return err
	}
	var completed struct {
		StopReason string `json:"stopReason"`
	}
	if err := c.call(ctx, "session/prompt", map[string]interface{}{
		"sessionId": session.SessionID,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	}, &completed); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	if completed.StopReason != "end_turn" {
		return fmt.Errorf("agent stopped with %q", c.req.Redactor.Redact(completed.StopReason))
	}
	return nil
}

func (c *client) call(ctx context.Context, method string, params interface{}, target interface{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.nextID++
	id := c.nextID
	if err := c.write(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return err
	}
	for c.scan.Scan() {
		line := append([]byte(nil), c.scan.Bytes()...)
		if err := c.record(line); err != nil {
			return err
		}
		var message wireMessage
		if err := json.Unmarshal(line, &message); err != nil {
			return fmt.Errorf("decode JSON-RPC line: %w", err)
		}
		if message.Method != "" {
			if len(message.ID) != 0 {
				if err := c.handleRequest(ctx, message); err != nil {
					return err
				}
			} else if err := c.handleNotification(message); err != nil {
				return err
			}
			continue
		}
		if !sameID(message.ID, id) {
			continue
		}
		if message.Error != nil {
			safe := c.req.Redactor.Redact(message.Error.Message)
			if authenticationError(message.Error.Code, safe) {
				return fmt.Errorf("%w: JSON-RPC %d: %s", ErrAuthenticationRequired, message.Error.Code, safe)
			}
			return fmt.Errorf("JSON-RPC %d: %s", message.Error.Code, safe)
		}
		if target == nil || len(message.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(message.Result, target); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.scan.Err(); err != nil {
		return fmt.Errorf("read ACP stream: %w", err)
	}
	return io.ErrUnexpectedEOF
}

func authenticationError(code int, message string) bool {
	if code == -32000 {
		return true
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "failed to authenticate") ||
		strings.Contains(lower, "authentication required") ||
		strings.Contains(lower, "oauth access token has expired") ||
		(strings.Contains(lower, "401") && strings.Contains(lower, "token"))
}

func (c *client) handleRequest(ctx context.Context, message wireMessage) error {
	if message.Method != "session/request_permission" {
		return c.writeError(message.ID, -32601, "method not supported by Ouvrier ACP client")
	}
	var params struct {
		ToolCall struct {
			ToolCallID string          `json:"toolCallId"`
			Kind       string          `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return c.writeError(message.ID, -32602, "invalid permission request")
	}
	if err := ctx.Err(); err != nil {
		return c.write(map[string]interface{}{
			"jsonrpc": "2.0", "id": json.RawMessage(message.ID),
			"result": map[string]interface{}{"outcome": map[string]string{"outcome": "cancelled"}},
		})
	}
	kind := strings.ToLower(strings.TrimSpace(params.ToolCall.Kind))
	if kind == "" {
		kind = c.tools[params.ToolCall.ToolCallID].Kind
	}
	allow := c.permissionAllowed(kind, params.ToolCall.RawInput)
	wanted := "allow_once"
	if !allow {
		wanted = "reject_once"
	}
	optionID := ""
	for _, option := range params.Options {
		if option.Kind == wanted {
			optionID = option.OptionID
			break
		}
	}
	if optionID == "" && !allow {
		for _, option := range params.Options {
			if option.Kind == "reject_always" {
				optionID = option.OptionID
				break
			}
		}
	}
	outcome := map[string]string{"outcome": "cancelled"}
	if optionID != "" {
		outcome = map[string]string{"outcome": "selected", "optionId": optionID}
	}
	return c.write(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(message.ID),
		"result": map[string]interface{}{"outcome": outcome},
	})
}

func (c *client) write(value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxProtocolLineBytes-1 {
		return fmt.Errorf("outbound ACP message exceeds %d bytes", maxProtocolLineBytes)
	}
	data = append(data, '\n')
	_, err = c.writer.Write(data)
	return err
}

func (c *client) writeError(id json.RawMessage, code int, message string) error {
	return c.write(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]interface{}{"code": code, "message": message},
	})
}

func (c *client) record(line []byte) error {
	separator := 0
	if c.rawBytes > 0 {
		separator = 1
	}
	if len(line) > maxProtocolBytes-separator-c.rawBytes {
		return fmt.Errorf("cumulative ACP output exceeds %d bytes", maxProtocolBytes)
	}
	c.rawBytes += separator + len(line)
	return nil
}

func (c *client) emit(event operate.Event) error {
	if c.sink == nil {
		return nil
	}
	return c.sink.Event(event)
}

func (c *client) result() operate.TurnResult {
	safe := strings.TrimSpace(c.text.String())
	// RawOutput deliberately contains only the normalized, continuously
	// redacted assistant stream. Persisting JSON-RPC frames would reassemble
	// transport metadata around secrets split over distinct JSON messages.
	return operate.TurnResult{FinalMessage: safe, RawOutput: safe}
}

func sameID(raw json.RawMessage, expected int64) bool {
	if len(raw) == 0 {
		return false
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return number == expected
	}
	var text string
	return json.Unmarshal(raw, &text) == nil && text == strconv.FormatInt(expected, 10)
}
