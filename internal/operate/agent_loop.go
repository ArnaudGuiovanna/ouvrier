package operate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// maxAgentSteps bounds one operator turn so a misbehaving model cannot loop
// forever calling tools.
const maxAgentSteps = 16

// AgentModel is the model transport for the Ouvrier-owned tool-calling loop.
// Unlike the Codex exec driver (which owns its own sandbox and tools), an
// AgentModel lets Ouvrier drive the tool loop: it returns assistant text and/or
// tool calls, and Ouvrier executes the tools through its governed registry.
//
// onDelta receives streaming assistant text; it may be called zero times for
// providers without streaming.
type AgentModel interface {
	Complete(ctx context.Context, req provider.Request, onDelta func(string)) (provider.Response, error)
}

// NewProviderModel adapts an internal/provider Provider into an AgentModel bound
// to a model id (e.g. "anthropic/claude-sonnet-4-6").
func NewProviderModel(p provider.Provider, model string) AgentModel {
	return providerModel{provider: p, model: model}
}

type providerModel struct {
	provider provider.Provider
	model    string
}

func (m providerModel) Complete(ctx context.Context, req provider.Request, onDelta func(string)) (provider.Response, error) {
	if strings.TrimSpace(req.Model) == "" {
		req.Model = m.model
	}
	if sp, ok := m.provider.(provider.StreamingProvider); ok && onDelta != nil {
		return sp.CompleteStream(ctx, req, func(d provider.Delta) { onDelta(d.Text) })
	}
	return m.provider.Complete(ctx, req)
}

// runAgentLoop drives a real model tool-calling loop for one operator turn. The
// user prompt has already been appended to the transcript and emitted by the
// caller. It runs until the model stops requesting tools, the step budget is
// exhausted, or the context is cancelled (Esc).
func (r *AgentRuntime) runAgentLoop(ctx context.Context, session *Session, turn *RuntimeTurn, emit func(StreamEvent)) (RuntimeTurn, error) {
	transcript, err := ReadTranscript(session.TranscriptPath)
	if err != nil {
		return *turn, err
	}
	msgs := historyMessages(transcript)
	if len(msgs) == 0 {
		// Should not happen: the caller appended the user entry first.
		return *turn, fmt.Errorf("operate: empty conversation")
	}
	system := ouvrierSystemPrompt(r.workspace)
	tools := r.toolSpecs()

	appendAssistant := func(text string) error {
		entry, err := r.transcript(session).Append(TranscriptEntry{
			SessionID: session.ID, Kind: TranscriptAssistant, Role: "assistant", Text: text,
		})
		if err != nil {
			return err
		}
		turn.Entries = append(turn.Entries, entry)
		emit(StreamEvent{Kind: StreamAssistant, Entry: &entry})
		return nil
	}
	finish := func(final string, runErr error) (RuntimeTurn, error) {
		turn.Final = final
		turn.Workspace = r.workspace
		emit(StreamEvent{Kind: StreamDone, Final: final, Workspace: r.workspace, Err: runErr})
		return *turn, runErr
	}

	for step := 0; step < maxAgentSteps; step++ {
		if err := ctx.Err(); err != nil {
			return finish("interrupted", err)
		}
		var streamed strings.Builder
		resp, err := r.Options.Model.Complete(ctx, provider.Request{
			Model:    r.Options.ModelID,
			System:   system,
			Messages: msgs,
			Tools:    tools,
		}, func(d string) {
			if d == "" {
				return
			}
			streamed.WriteString(d)
			emit(StreamEvent{Kind: StreamAssistantDelta, Delta: d})
		})
		if err != nil {
			msg := "model error: " + err.Error()
			entry, _ := r.transcript(session).Append(TranscriptEntry{SessionID: session.ID, Kind: TranscriptError, Role: "assistant", Text: msg})
			turn.Entries = append(turn.Entries, entry)
			emit(StreamEvent{Kind: StreamError, Entry: &entry, Err: err})
			return finish(msg, err)
		}

		text := strings.TrimSpace(resp.Text)
		if text == "" {
			text = strings.TrimSpace(streamed.String())
		}

		if len(resp.ToolCalls) == 0 {
			if text != "" {
				if err := appendAssistant(text); err != nil {
					return *turn, err
				}
			}
			return finish(text, nil)
		}

		// Assistant turn requested tools: show any preamble, then execute.
		if text != "" {
			if err := appendAssistant(text); err != nil {
				return *turn, err
			}
		}
		msgs = append(msgs, provider.AssistantToolCalls(resp.Text, resp.ToolCalls...))

		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return finish("interrupted", err)
			}
			input := map[string]any{}
			if len(call.Arguments) > 0 {
				_ = json.Unmarshal(call.Arguments, &input)
			}
			result, runErr := r.callTool(ctx, session, plannedTool{ID: call.ID, Name: call.Name, Input: input}, turn, emit)
			msgs = append(msgs, provider.ToolResultText(
				provider.ToolCall{ID: call.ID, Name: call.Name},
				toolResultContent(result, runErr),
				runErr != nil,
			))
		}
	}
	return finish("reached the maximum number of tool steps for this turn", nil)
}

// historyMessages rebuilds a provider conversation from the persisted
// transcript, including tool-call/result turns paired by their stable
// tool_call_id. Assistant text that precedes a tool call is attached to the
// same assistant message so the history is provider-valid.
func historyMessages(entries []TranscriptEntry) []provider.Message {
	var msgs []provider.Message
	var pendingText string
	lastCallID := ""
	synth := 0

	flushText := func() {
		if t := strings.TrimSpace(pendingText); t != "" {
			msgs = append(msgs, provider.AssistantText(t))
		}
		pendingText = ""
	}

	for _, e := range entries {
		switch e.Kind {
		case TranscriptUser:
			flushText()
			if t := strings.TrimSpace(e.Text); t != "" {
				msgs = append(msgs, provider.UserText(t))
			}
		case TranscriptAssistant:
			if t := strings.TrimSpace(e.Text); t != "" {
				pendingText = t
			}
		case TranscriptToolCall:
			id := metaString(e.Metadata, "tool_call_id")
			if id == "" {
				synth++
				id = fmt.Sprintf("call_%d", synth)
			}
			lastCallID = id
			args := json.RawMessage(`{}`)
			if len(e.Input) > 0 {
				if b, err := json.Marshal(e.Input); err == nil {
					args = b
				}
			}
			msgs = append(msgs, provider.AssistantToolCalls(
				strings.TrimSpace(pendingText),
				provider.ToolCall{ID: id, Name: e.ToolName, Arguments: args},
			))
			pendingText = ""
		case TranscriptToolResult:
			id := metaString(e.Metadata, "tool_call_id")
			if id == "" {
				id = lastCallID
			}
			msgs = append(msgs, provider.ToolResultText(
				provider.ToolCall{ID: id, Name: e.ToolName},
				toolResultContentFromOutput(e.Output),
				outputIsError(e.Output),
			))
		}
	}
	flushText()
	return msgs
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func outputIsError(out map[string]any) bool {
	if out == nil {
		return false
	}
	_, ok := out["error"]
	return ok
}

func toolResultContentFromOutput(out map[string]any) string {
	if out == nil {
		return "done"
	}
	if e, ok := out["error"].(string); ok && strings.TrimSpace(e) != "" {
		return "error: " + e
	}
	data, err := json.Marshal(out)
	if err != nil {
		if s, ok := out["summary"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		return "done"
	}
	const limit = 8 * 1024
	if len(data) > limit {
		return string(data[:limit])
	}
	return string(data)
}

func (r *AgentRuntime) toolSpecs() []provider.ToolSpec {
	names := r.Tools.Names()
	specs := make([]provider.ToolSpec, 0, len(names))
	for _, name := range names {
		tool, ok := r.Tools.Tool(name)
		if !ok {
			continue
		}
		specs = append(specs, provider.ToolSpec{
			Name:        name,
			Description: tool.Description,
			InputSchema: toolSchema(name),
		})
	}
	return specs
}

func toolResultContent(result ToolResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	payload := map[string]any{"summary": result.Summary}
	for k, v := range result.Data {
		payload[k] = v
	}
	data, mErr := json.Marshal(payload)
	if mErr != nil {
		return result.Summary
	}
	const limit = 8 * 1024
	if len(data) > limit {
		return string(data[:limit])
	}
	return string(data)
}

// toolSchema returns a minimal JSON Schema for one native tool. Tools that take
// no input get an empty object schema.
func toolSchema(name string) json.RawMessage {
	if schema, ok := toolSchemas[name]; ok {
		return json.RawMessage(schema)
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

var toolSchemas = map[string]string{
	"scaffold_worker":     `{"type":"object","properties":{"name":{"type":"string","description":"worker directory/name"},"trigger":{"type":"string","description":"e.g. POST /tickets, cron, webhook, stream"},"model":{"type":"string","description":"provider/model id"}},"required":["name","trigger"]}`,
	"patch_worker":        `{"type":"object","properties":{"goal":{"type":"string","description":"what to implement or change"}},"required":["goal"]}`,
	"fix_worker":          `{"type":"object","properties":{"subject":{"type":"string","description":"finding or issue to repair"}}}`,
	"review_worker":       `{"type":"object","properties":{"subject":{"type":"string","description":"review focus, e.g. security and governance"}}}`,
	"read_worker_file":    `{"type":"object","properties":{"path":{"type":"string","description":"path relative to the worker, e.g. main.go"}},"required":["path"]}`,
	"search_ouvrier_docs": `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`,
	"build_worker":        `{"type":"object","properties":{"target":{"type":"string","description":"GOOS/GOARCH, e.g. linux/amd64"}}}`,
	"transfer_worker":     `{"type":"object","properties":{"env":{"type":"string","description":"deploy environment, e.g. staging"},"target":{"type":"string"},"env_file":{"type":"string"}},"required":["env"]}`,
	"accept_risk":         `{"type":"object","properties":{"rationale":{"type":"string"}},"required":["rationale"]}`,
}
