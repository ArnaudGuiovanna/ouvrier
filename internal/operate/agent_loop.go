package operate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// maxAgentSteps bounds one operator turn so a misbehaving model cannot loop
// forever calling tools.
const maxAgentSteps = 16

const (
	maxAgentToolCalls        = 64
	maxAgentToolCallsPerStep = 16
	maxAgentToolCallIDBytes  = 256
	maxAgentToolNameBytes    = 256
	maxAgentToolArgsBytes    = 1 << 20
	maxAgentModelTextBytes   = 1 << 20
)

const (
	toolCallGroupIDKey    = "assistant_tool_group_id"
	toolCallGroupIndexKey = "assistant_tool_group_index"
	toolCallGroupCountKey = "assistant_tool_group_count"
)

// ErrAgentLoopExhausted marks a bounded model loop that did not reach a valid
// terminal condition. Callers must not treat the last model text as success.
var ErrAgentLoopExhausted = errors.New("operate: agent loop outcome exhausted")

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

type agentModelTurnAborter interface {
	AbortTurn(context.Context) error
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

func (m providerModel) Close() error {
	if closer, ok := m.provider.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

func (m providerModel) AbortTurn(ctx context.Context) error {
	if aborter, ok := m.provider.(interface {
		AbortTurn(context.Context) error
	}); ok {
		return aborter.AbortTurn(ctx)
	}
	return nil
}

// runAgentLoop drives a real model tool-calling loop for one operator turn. The
// user prompt has already been appended to the transcript and emitted by the
// caller. It runs until the model stops requesting tools, the step budget is
// exhausted, or the context is cancelled (Esc).
func (r *AgentRuntime) runAgentLoop(ctx context.Context, session *Session, turn *RuntimeTurn, emit func(StreamEvent), ctrl *turnControl) (result RuntimeTurn, retErr error) {
	completed := false
	defer func() {
		if completed {
			return
		}
		aborter, ok := r.Options.Model.(agentModelTurnAborter)
		if !ok {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := aborter.AbortTurn(abortCtx); err != nil {
			abortErr := redactError(r.Options.Redactor, fmt.Errorf("operate: abort incomplete model turn: %w", err))
			retErr = errors.Join(retErr, abortErr)
		}
	}()
	transcript, err := ReadTranscript(session.TranscriptPath)
	if err != nil {
		return *turn, err
	}
	msgs, err := historyMessages(transcript)
	if err != nil {
		return *turn, err
	}
	if len(msgs) == 0 {
		// Should not happen: the caller appended the user entry first.
		return *turn, fmt.Errorf("operate: empty conversation")
	}
	tools := r.toolSpecs()

	appendAssistant := func(text string, metadata map[string]any) (TranscriptEntry, error) {
		entry, err := r.appendTranscript(session, TranscriptEntry{
			SessionID: session.ID, Kind: TranscriptAssistant, Role: "assistant", Text: text, Metadata: metadata,
		})
		if err != nil {
			return TranscriptEntry{}, err
		}
		turn.Entries = append(turn.Entries, entry)
		if strings.TrimSpace(text) != "" {
			emit(StreamEvent{Kind: StreamAssistant, Entry: &entry})
		}
		return entry, nil
	}
	finish := func(final string, runErr error) (RuntimeTurn, error) {
		workspace := workspaceForSession(session)
		turn.Final = final
		turn.Workspace = workspace
		emit(StreamEvent{Kind: StreamDone, Final: final, Outcome: turn.Outcome, Workspace: workspace, Err: runErr})
		return *turn, runErr
	}
	completion := completionGateFromTranscript(transcript, session)
	if mutationIntent(lastUserPrompt(transcript)) {
		completion.requireMutationIntent()
	}
	seenToolCallIDs, err := persistedToolCallIDs(transcript)
	if err != nil {
		return *turn, err
	}
	totalToolCalls := 0

	for step := 0; step < maxAgentSteps; step++ {
		if err := ctx.Err(); err != nil {
			return finish("interrupted", err)
		}
		var streamed strings.Builder
		streamOverflow := false
		streamRedactor := r.Options.Redactor.stream()
		modelCtx, cancelModel := context.WithCancel(ctx)
		resp, err := r.Options.Model.Complete(modelCtx, provider.Request{
			Model:    r.Options.ModelID,
			System:   r.Options.Redactor.Redact(ouvrierSystemPrompt(workspaceForSession(session))),
			Messages: msgs,
			Tools:    tools,
		}, func(d string) {
			if d == "" || streamOverflow {
				return
			}
			if len(d) > maxAgentModelTextBytes-streamed.Len() {
				streamOverflow = true
				cancelModel()
				return
			}
			streamed.WriteString(d)
			if safe := streamRedactor.Push(d); safe != "" {
				emit(StreamEvent{Kind: StreamAssistantDelta, Delta: safe})
			}
		})
		cancelModel()
		if tail := streamRedactor.Flush(); tail != "" {
			emit(StreamEvent{Kind: StreamAssistantDelta, Delta: tail})
		}
		if streamOverflow {
			err = fmt.Errorf("operate: model streamed more than %d text bytes", maxAgentModelTextBytes)
		} else if len(resp.Text) > maxAgentModelTextBytes {
			err = fmt.Errorf("operate: model response text exceeds %d bytes", maxAgentModelTextBytes)
		}
		if err != nil {
			msg := "model error: " + err.Error()
			entry, persistErr := r.appendTranscript(session, TranscriptEntry{SessionID: session.ID, Kind: TranscriptError, Role: "assistant", Text: msg})
			if persistErr != nil {
				return finish(r.Redact(msg), errors.Join(redactError(r.Options.Redactor, err), persistErr))
			}
			turn.Entries = append(turn.Entries, entry)
			redactedErr := redactError(r.Options.Redactor, err)
			emit(StreamEvent{Kind: StreamError, Entry: &entry, Err: redactedErr})
			return finish(r.Redact(msg), redactedErr)
		}

		text := strings.TrimSpace(resp.Text)
		if text == "" {
			text = strings.TrimSpace(streamed.String())
		}
		safeAssistantText := r.Options.Redactor.Redact(text)

		if len(resp.ToolCalls) == 0 {
			// The model turn can take arbitrarily long and external/operator
			// processes may mutate source or evidence while it is in flight.
			// Re-read the exact source/audit/build chain at the last possible
			// point before accepting a successful outcome. Structured tool
			// results alone are never durable completion proof.
			completion.reconcilePersistedEvidence(session)
			if !completion.complete() {
				if text != "" {
					if _, err := appendAssistant(text, nil); err != nil {
						return *turn, err
					}
					msgs = append(msgs, provider.AssistantText(safeAssistantText))
				}
				request, err := completion.request()
				if err != nil {
					return finish("completion evidence request failed", err)
				}
				entry, err := r.appendTranscript(session, TranscriptEntry{
					SessionID: session.ID,
					Kind:      TranscriptStatus,
					Text:      request,
					Metadata: map[string]any{
						"completion_gate":  "incomplete",
						"missing_evidence": completion.missingEvidence(),
					},
				})
				if err != nil {
					return finish("completion evidence request could not be persisted", err)
				}
				turn.Entries = append(turn.Entries, entry)
				emit(StreamEvent{Kind: StreamStatus, Entry: &entry, Final: request})
				msgs = append(msgs, provider.UserText(request))
				continue
			}
			if text != "" {
				if _, err := appendAssistant(text, nil); err != nil {
					return *turn, err
				}
			}
			completed = true
			return finish(text, nil)
		}
		if len(resp.ToolCalls) > maxAgentToolCallsPerStep || totalToolCalls > maxAgentToolCalls-len(resp.ToolCalls) {
			runErr := fmt.Errorf("operate: model requested too many tools (%d in one step, %d total limit)", len(resp.ToolCalls), maxAgentToolCalls)
			return finish(runErr.Error(), runErr)
		}

		// Validate the complete assistant response before making any part of it
		// executable. This prevents a valid first call from running when a later
		// sibling has a malformed/duplicate ID or invalid arguments.
		prepared := make([]plannedTool, len(resp.ToolCalls))
		stepIDs := make(map[string]struct{}, len(resp.ToolCalls))
		for i, call := range resp.ToolCalls {
			if len(call.Name) == 0 || len(call.Name) > maxAgentToolNameBytes {
				runErr := fmt.Errorf("operate: model tool name must contain 1..%d bytes", maxAgentToolNameBytes)
				return finish(runErr.Error(), runErr)
			}
			if len(call.Arguments) > maxAgentToolArgsBytes {
				runErr := fmt.Errorf("operate: model tool %q arguments exceed %d bytes", boundedModelText(call.Name), maxAgentToolArgsBytes)
				return finish(runErr.Error(), runErr)
			}
			if err := validateAgentToolCallID(call.ID); err != nil {
				runErr := fmt.Errorf("operate: model tool %q has an invalid stable call id: %w", boundedModelText(call.Name), err)
				return finish(runErr.Error(), runErr)
			}
			if r.Options.Redactor.Redact(call.ID) != call.ID {
				runErr := fmt.Errorf("operate: model tool %q call id contains protected data", boundedModelText(call.Name))
				return finish(runErr.Error(), runErr)
			}
			if _, duplicate := seenToolCallIDs[call.ID]; duplicate {
				runErr := fmt.Errorf("operate: duplicate model tool call id %q", call.ID)
				return finish(runErr.Error(), runErr)
			}
			if _, duplicate := stepIDs[call.ID]; duplicate {
				runErr := fmt.Errorf("operate: duplicate model tool call id %q", call.ID)
				return finish(runErr.Error(), runErr)
			}
			stepIDs[call.ID] = struct{}{}
			tool, callable := r.Tools.Tool(call.Name)
			if !callable || tool.OperatorOnly {
				validationErr := fmt.Errorf("operate: tool %q is not callable by the model", call.Name)
				runErr := errors.New(r.Options.Redactor.Redact(validationErr.Error()))
				entry, persistErr := r.appendTranscript(session, TranscriptEntry{
					SessionID: session.ID,
					Kind:      TranscriptError,
					Role:      "assistant",
					Text:      runErr.Error(),
					ToolName:  call.Name,
					Metadata:  map[string]any{"tool_call_id": call.ID, "model_callable": false},
				})
				if persistErr != nil {
					return finish(runErr.Error(), errors.Join(runErr, persistErr))
				}
				turn.Entries = append(turn.Entries, entry)
				emit(StreamEvent{Kind: StreamError, Entry: &entry, Err: runErr})
				return finish(runErr.Error(), runErr)
			}
			input, err := decodeModelToolArguments(call.Name, call.Arguments)
			if err != nil {
				validationErr := fmt.Errorf("operate: tool %q arguments are invalid: %w", call.Name, err)
				runErr := errors.New(r.Options.Redactor.Redact(validationErr.Error()))
				entry, persistErr := r.appendTranscript(session, TranscriptEntry{
					SessionID: session.ID,
					Kind:      TranscriptError,
					Role:      "assistant",
					Text:      runErr.Error(),
					ToolName:  call.Name,
					Metadata:  map[string]any{"tool_call_id": call.ID},
				})
				if persistErr != nil {
					return finish(runErr.Error(), errors.Join(runErr, persistErr))
				}
				turn.Entries = append(turn.Entries, entry)
				emit(StreamEvent{Kind: StreamError, Entry: &entry, Err: runErr})
				return finish(runErr.Error(), runErr)
			}
			prepared[i] = plannedTool{ID: call.ID, Name: call.Name, Input: input}
		}

		totalToolCalls += len(prepared)
		groupID, err := randomID()
		if err != nil {
			return finish("tool-call group id generation failed", err)
		}
		if _, err := appendAssistant(text, map[string]any{
			toolCallGroupIDKey:    groupID,
			toolCallGroupCountKey: len(prepared),
		}); err != nil {
			return *turn, err
		}

		// Persist every sibling call before the first one can execute. A crash
		// therefore leaves a fully recoverable group: startup closes each call
		// without a result as interrupted, and replay retains the exact IDs.
		for i := range prepared {
			entry, err := r.appendTranscript(session, TranscriptEntry{
				SessionID: session.ID,
				Kind:      TranscriptToolCall,
				ToolName:  prepared[i].Name,
				Input:     prepared[i].Input,
				Metadata: map[string]any{
					"tool_call_id":        prepared[i].ID,
					toolCallGroupIDKey:    groupID,
					toolCallGroupIndexKey: i,
					toolCallGroupCountKey: len(prepared),
				},
			})
			if err != nil {
				return finish("tool-call group could not be persisted", err)
			}
			turn.Entries = append(turn.Entries, entry)
			prepared[i].persistedEntry = &entry
			seenToolCallIDs[prepared[i].ID] = struct{}{}
		}
		msgs = append(msgs, provider.AssistantToolCalls(safeAssistantText, resp.ToolCalls...))

		toolResultBlocks := make([]provider.Block, 0, len(resp.ToolCalls))
		for i, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return finish("interrupted", err)
			}
			completion.observeAttempt(call.Name)
			result, runErr := r.callTool(ctx, session, prepared[i], turn, emit, ctrl)
			completion.observe(call.Name, result, runErr)
			safeResult := redactToolResult(r.Options.Redactor, result)
			safeErr := redactError(r.Options.Redactor, runErr)
			resultMessage := provider.ToolResultText(
				provider.ToolCall{ID: call.ID, Name: call.Name},
				toolResultContent(safeResult, safeErr),
				safeErr != nil,
			)
			toolResultBlocks = append(toolResultBlocks, resultMessage.Blocks...)
		}
		msgs = append(msgs, provider.Message{Role: provider.RoleTool, Blocks: toolResultBlocks})
	}
	turn.Outcome = RuntimeOutcomeExhausted
	runErr := fmt.Errorf("%w after %d model steps", ErrAgentLoopExhausted, maxAgentSteps)
	entry, persistErr := r.appendTranscript(session, TranscriptEntry{
		SessionID: session.ID,
		Kind:      TranscriptError,
		Role:      "assistant",
		Text:      runErr.Error(),
		Metadata: map[string]any{
			"outcome":   string(RuntimeOutcomeExhausted),
			"max_steps": maxAgentSteps,
		},
	})
	if persistErr == nil {
		turn.Entries = append(turn.Entries, entry)
		emit(StreamEvent{Kind: StreamError, Entry: &entry, Err: runErr})
	} else {
		runErr = errors.Join(runErr, persistErr)
	}
	return finish(runErr.Error(), runErr)
}

func lastUserPrompt(entries []TranscriptEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == TranscriptUser {
			return entries[i].Text
		}
	}
	return ""
}

func redactToolResult(redactor Redactor, result ToolResult) ToolResult {
	result.Summary = redactor.Redact(result.Summary)
	result.Data = redactMap(redactor, result.Data)
	return result
}

// historyMessages rebuilds a provider conversation from the persisted
// transcript. Calls emitted by one assistant response are replayed in one
// assistant message followed by their results, which is required by providers
// such as OpenAI and Anthropic. New transcripts carry an explicit durable
// group id; the legacy fallback uses durable call/result boundaries because
// older journals did not record assistant group bounds.
func historyMessages(entries []TranscriptEntry) ([]provider.Message, error) {
	entries, compactedContext, err := compactModelHistory(entries)
	if err != nil {
		return nil, err
	}
	entries, err = normalizeLegacyToolCallIDs(entries)
	if err != nil {
		return nil, err
	}
	type assistantGroup struct {
		id        string
		expected  int
		text      string
		calls     []provider.ToolCall
		callNames map[string]string
		results   []provider.Block
		resultIDs map[string]struct{}
	}

	var msgs []provider.Message
	if compactedContext != "" {
		msgs = append(msgs, provider.UserText(compactedContext))
	}
	var current *assistantGroup
	seenCallIDs := make(map[string]struct{})

	flush := func() error {
		if current == nil {
			return nil
		}
		text := strings.TrimSpace(current.text)
		if len(current.calls) == 0 {
			// A crash can happen after the group marker is synced but before
			// its first call record. Preserve any preamble and safely omit calls
			// that were never made durable or executable.
			if text != "" {
				msgs = append(msgs, provider.AssistantText(text))
			}
			current = nil
			return nil
		}
		if current.expected > 0 && len(current.calls) > current.expected {
			return fmt.Errorf("operate: transcript tool-call group %q has %d calls, exceeds declared count %d", current.id, len(current.calls), current.expected)
		}
		if len(current.calls) > maxAgentToolCallsPerStep {
			return fmt.Errorf("operate: transcript assistant group has too many tools (%d, limit %d)", len(current.calls), maxAgentToolCallsPerStep)
		}
		if len(current.results) != len(current.calls) {
			return fmt.Errorf("operate: transcript assistant group has %d calls and %d results", len(current.calls), len(current.results))
		}
		msgs = append(msgs, provider.AssistantToolCalls(text, current.calls...))
		msgs = append(msgs, provider.Message{Role: provider.RoleTool, Blocks: append([]provider.Block(nil), current.results...)})
		current = nil
		return nil
	}

	startGroup := func(text, id string, expected int) {
		current = &assistantGroup{
			id: id, expected: expected, text: text,
			callNames: make(map[string]string), resultIDs: make(map[string]struct{}),
		}
	}

	for index, entry := range entries {
		switch entry.Kind {
		case TranscriptUser:
			if err := flush(); err != nil {
				return nil, err
			}
			if text := strings.TrimSpace(entry.Text); text != "" {
				msgs = append(msgs, provider.UserText(text))
			}

		case TranscriptAssistant:
			groupID := metaString(entry.Metadata, toolCallGroupIDKey)
			if groupID == "" {
				if err := flush(); err != nil {
					return nil, err
				}
				startGroup(entry.Text, "", 0)
				continue
			}
			if err := validateAgentToolCallID(groupID); err != nil {
				return nil, fmt.Errorf("operate: transcript assistant group id is invalid: %w", err)
			}
			expected, ok := metaInt(entry.Metadata, toolCallGroupCountKey)
			if !ok || expected < 1 || expected > maxAgentToolCallsPerStep {
				return nil, fmt.Errorf("operate: transcript assistant group %q has invalid call count", groupID)
			}
			if err := flush(); err != nil {
				return nil, err
			}
			startGroup(entry.Text, groupID, expected)

		case TranscriptToolCall:
			id := metaString(entry.Metadata, "tool_call_id")
			if err := validateAgentToolCallID(id); err != nil {
				return nil, fmt.Errorf("operate: transcript tool call at entry %d has invalid id: %w", index, err)
			}
			if _, duplicate := seenCallIDs[id]; duplicate {
				return nil, fmt.Errorf("operate: transcript has duplicate tool call id %q", id)
			}
			groupID := metaString(entry.Metadata, toolCallGroupIDKey)
			if groupID != "" {
				if err := validateAgentToolCallID(groupID); err != nil {
					return nil, fmt.Errorf("operate: transcript tool-call group id is invalid: %w", err)
				}
				expected, ok := metaInt(entry.Metadata, toolCallGroupCountKey)
				if !ok || expected < 1 || expected > maxAgentToolCallsPerStep {
					return nil, fmt.Errorf("operate: transcript tool call %q has invalid group count", id)
				}
				callIndex, ok := metaInt(entry.Metadata, toolCallGroupIndexKey)
				if !ok || callIndex < 0 || callIndex >= expected {
					return nil, fmt.Errorf("operate: transcript tool call %q has invalid group index", id)
				}
				if current == nil {
					startGroup("", groupID, expected)
				} else if current.id == "" && len(current.calls) == 0 {
					current.id, current.expected = groupID, expected
				} else if current.id != groupID {
					if err := flush(); err != nil {
						return nil, err
					}
					startGroup("", groupID, expected)
				}
				if current.expected != expected || callIndex != len(current.calls) {
					return nil, fmt.Errorf("operate: transcript tool call %q is out of order in group %q", id, groupID)
				}
			} else {
				// Old transcripts have no group marker. Once one historical
				// call has a result, the next call begins a new assistant turn.
				if current != nil && (current.id != "" || len(current.results) > 0) {
					if err := flush(); err != nil {
						return nil, err
					}
				}
				if current == nil {
					startGroup("", "", 0)
				}
			}
			if len(current.calls) >= maxAgentToolCallsPerStep {
				return nil, fmt.Errorf("operate: transcript assistant group exceeds %d tool calls", maxAgentToolCallsPerStep)
			}
			arguments := json.RawMessage(`{}`)
			if len(entry.Input) > 0 {
				encoded, err := json.Marshal(entry.Input)
				if err != nil {
					return nil, fmt.Errorf("operate: encode transcript tool call %q: %w", id, err)
				}
				arguments = encoded
			}
			call := provider.ToolCall{ID: id, Name: entry.ToolName, Arguments: arguments}
			if err := call.Validate(); err != nil {
				return nil, fmt.Errorf("operate: invalid transcript tool call %q: %w", id, err)
			}
			if _, duplicate := current.callNames[id]; duplicate {
				return nil, fmt.Errorf("operate: transcript assistant group has duplicate tool call id %q", id)
			}
			current.calls = append(current.calls, call)
			current.callNames[id] = entry.ToolName
			seenCallIDs[id] = struct{}{}

		case TranscriptToolResult:
			id := metaString(entry.Metadata, "tool_call_id")
			if err := validateAgentToolCallID(id); err != nil {
				return nil, fmt.Errorf("operate: transcript tool result at entry %d has invalid id: %w", index, err)
			}
			if current == nil {
				return nil, fmt.Errorf("operate: transcript tool result %q has no assistant group", id)
			}
			name, ok := current.callNames[id]
			if !ok {
				return nil, fmt.Errorf("operate: transcript tool result %q has no matching call in its assistant group", id)
			}
			if entry.ToolName != "" && entry.ToolName != name {
				return nil, fmt.Errorf("operate: transcript tool result %q names %q, want %q", id, entry.ToolName, name)
			}
			if _, duplicate := current.resultIDs[id]; duplicate {
				return nil, fmt.Errorf("operate: transcript has duplicate tool result id %q", id)
			}
			current.resultIDs[id] = struct{}{}
			resultMessage := provider.ToolResultText(
				provider.ToolCall{ID: id, Name: name},
				toolResultContentFromOutput(entry.Output),
				outputIsError(entry.Output),
			)
			current.results = append(current.results, resultMessage.Blocks...)

		case TranscriptStatus:
			if metaString(entry.Metadata, "completion_gate") == "incomplete" {
				if err := flush(); err != nil {
					return nil, err
				}
				if text := strings.TrimSpace(entry.Text); text != "" {
					msgs = append(msgs, provider.UserText(text))
				}
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	for index, message := range msgs {
		if err := message.Validate(); err != nil {
			return nil, fmt.Errorf("operate: reconstructed provider message %d is invalid: %w", index, err)
		}
	}
	return msgs, nil
}

func persistedToolCallIDs(entries []TranscriptEntry) (map[string]struct{}, error) {
	entries, err := normalizeLegacyToolCallIDs(entries)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{})
	for index, entry := range entries {
		if entry.Kind != TranscriptToolCall {
			continue
		}
		id := metaString(entry.Metadata, "tool_call_id")
		if err := validateAgentToolCallID(id); err != nil {
			return nil, fmt.Errorf("operate: transcript tool call at entry %d has invalid id: %w", index, err)
		}
		if _, duplicate := ids[id]; duplicate {
			return nil, fmt.Errorf("operate: transcript has duplicate tool call id %q", id)
		}
		ids[id] = struct{}{}
	}
	return ids, nil
}

type pendingLegacyToolCall struct {
	id     string
	tool   string
	legacy bool
}

// normalizeLegacyToolCallIDs upgrades old in-memory transcript entries that
// predate durable tool_call_id metadata. The synthesized ID is deterministic
// from immutable entry content and position, so every resume reconstructs the
// same provider correlation. Explicit/new grouped IDs remain strict.
func normalizeLegacyToolCallIDs(entries []TranscriptEntry) ([]TranscriptEntry, error) {
	normalized := append([]TranscriptEntry(nil), entries...)
	seen := make(map[string]struct{})
	pending := make([]pendingLegacyToolCall, 0)
	for index := range normalized {
		entry := &normalized[index]
		switch entry.Kind {
		case TranscriptToolCall:
			id := metaString(entry.Metadata, "tool_call_id")
			legacy := id == ""
			if legacy {
				if metaString(entry.Metadata, toolCallGroupIDKey) != "" {
					return nil, fmt.Errorf("operate: grouped transcript tool call at entry %d has no stable id", index)
				}
				id = syntheticLegacyToolCallID(*entry, index)
				entry.Metadata = cloneMetadata(entry.Metadata)
				entry.Metadata["tool_call_id"] = id
				entry.Metadata["legacy_synthetic_tool_call_id"] = true
			}
			if err := validateAgentToolCallID(id); err != nil {
				return nil, fmt.Errorf("operate: transcript tool call at entry %d has invalid id: %w", index, err)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("operate: transcript has duplicate tool call id %q", id)
			}
			seen[id] = struct{}{}
			pending = append(pending, pendingLegacyToolCall{id: id, tool: entry.ToolName, legacy: legacy})

		case TranscriptToolResult:
			id := metaString(entry.Metadata, "tool_call_id")
			pendingIndex := -1
			if id == "" {
				for candidate := range pending {
					if entry.ToolName == "" || pending[candidate].tool == entry.ToolName {
						pendingIndex = candidate
						break
					}
				}
				if pendingIndex < 0 || !pending[pendingIndex].legacy {
					return nil, fmt.Errorf("operate: transcript tool result at entry %d has no stable matching call id", index)
				}
				id = pending[pendingIndex].id
				entry.Metadata = cloneMetadata(entry.Metadata)
				entry.Metadata["tool_call_id"] = id
				entry.Metadata["legacy_synthetic_tool_call_id"] = true
			} else {
				if err := validateAgentToolCallID(id); err != nil {
					return nil, fmt.Errorf("operate: transcript tool result at entry %d has invalid id: %w", index, err)
				}
				for candidate := range pending {
					if pending[candidate].id == id {
						pendingIndex = candidate
						break
					}
				}
			}
			if pendingIndex >= 0 {
				pending = append(pending[:pendingIndex], pending[pendingIndex+1:]...)
			}
		}
	}
	return normalized, nil
}

func syntheticLegacyToolCallID(entry TranscriptEntry, index int) string {
	if entry.ID != "" {
		digest := sha256.Sum256([]byte("legacy-entry\x00" + entry.SessionID + "\x00" + entry.ID))
		return fmt.Sprintf("legacy_%x", digest[:12])
	}
	seed, err := json.Marshal(struct {
		SessionID string         `json:"session_id"`
		Index     int            `json:"index"`
		ToolName  string         `json:"tool_name"`
		Input     map[string]any `json:"input"`
	}{entry.SessionID, index, entry.ToolName, entry.Input})
	if err != nil {
		seed = []byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", entry.ID, entry.SessionID, index, entry.ToolName))
	}
	digest := sha256.Sum256(seed)
	return fmt.Sprintf("legacy_%x", digest[:12])
}

func cloneMetadata(metadata map[string]any) map[string]any {
	cloned := make(map[string]any, len(metadata)+2)
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func validateAgentToolCallID(id string) error {
	if id == "" || strings.TrimSpace(id) != id {
		return errors.New("tool call id must be non-empty and have no surrounding whitespace")
	}
	if len(id) > maxAgentToolCallIDBytes {
		return fmt.Errorf("tool call id exceeds %d bytes", maxAgentToolCallIDBytes)
	}
	if !utf8.ValidString(id) {
		return errors.New("tool call id must be valid UTF-8")
	}
	for _, char := range id {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return errors.New("tool call id must not contain whitespace or control characters")
		}
	}
	return nil
}

func metaInt(metadata map[string]any, key string) (int, bool) {
	if metadata == nil {
		return 0, false
	}
	switch value := metadata[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), int64(int(value)) == value
	case float64:
		converted := int(value)
		return converted, float64(converted) == value
	case json.Number:
		parsed, err := value.Int64()
		return int(parsed), err == nil && int64(int(parsed)) == parsed
	default:
		return 0, false
	}
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
		return boundedModelText("error: " + e)
	}
	fallback := "done"
	if summary, ok := out["summary"].(string); ok && strings.TrimSpace(summary) != "" {
		fallback = summary
	}
	return boundedToolResultJSON(out, fallback)
}

func (r *AgentRuntime) toolSpecs() []provider.ToolSpec {
	names := r.Tools.Names()
	specs := make([]provider.ToolSpec, 0, len(names))
	for _, name := range names {
		tool, ok := r.Tools.Tool(name)
		if !ok {
			continue
		}
		if tool.OperatorOnly {
			// Operator-only tools (IDE saves, the `!` shell) never reach the
			// model tool loop; they stay behind the GovernedExecutor.
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
		return boundedModelText("error: " + err.Error())
	}
	payload := map[string]any{"summary": result.Summary}
	for k, v := range result.Data {
		payload[k] = v
	}
	return boundedToolResultJSON(payload, result.Summary)
}

const maxModelToolResultBytes = 8 * 1024

func boundedToolResultJSON(value any, summary string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return boundedModelText(summary)
	}
	if len(data) <= maxModelToolResultBytes {
		return string(data)
	}
	summary = boundedModelText(summary)
	if len(summary) > 512 {
		summary = summary[:512]
	}
	preview := string(data[:maxModelToolResultBytes/2])
	for {
		envelope, marshalErr := json.Marshal(map[string]any{
			"summary": summary, "truncated": true, "original_bytes": len(data), "json_preview": preview,
		})
		if marshalErr == nil && len(envelope) <= maxModelToolResultBytes {
			return string(envelope)
		}
		if len(preview) == 0 {
			return `{"summary":"tool result truncated","truncated":true}`
		}
		preview = preview[:len(preview)/2]
	}
}

func boundedModelText(value string) string {
	if len(value) <= maxModelToolResultBytes {
		return value
	}
	value = value[:maxModelToolResultBytes-len("...[truncated]")]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "...[truncated]"
}

// toolSchema returns a minimal JSON Schema for one native tool. Tools that take
// no input get an empty object schema.
func toolSchema(name string) json.RawMessage {
	if schema, ok := toolSchemas[name]; ok {
		return json.RawMessage(schema)
	}
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

var toolSchemas = map[string]string{
	"scaffold_worker":     `{"type":"object","properties":{"name":{"type":"string","description":"worker directory/name"},"trigger":{"type":"string","description":"e.g. POST /tickets, cron, webhook, stream"},"model":{"type":"string","description":"provider/model id"}},"required":["name","trigger"],"additionalProperties":false}`,
	"patch_worker":        `{"type":"object","properties":{"goal":{"type":"string","description":"what to implement or change"}},"required":["goal"],"additionalProperties":false}`,
	"fix_worker":          `{"type":"object","properties":{"subject":{"type":"string","description":"finding or issue to repair"}},"additionalProperties":false}`,
	"review_worker":       `{"type":"object","properties":{"scope":{"type":"string","description":"review scope, e.g. whole_worker or governance_security"},"subject":{"type":"string","description":"review focus, e.g. security and governance"}},"additionalProperties":false}`,
	"read_worker_file":    `{"type":"object","properties":{"path":{"type":"string","description":"path relative to the worker, e.g. main.go"}},"required":["path"],"additionalProperties":false}`,
	"write_worker_file":   `{"type":"object","properties":{"path":{"type":"string","minLength":1,"description":"UTF-8 source path relative to the worker"},"content":{"type":"string","maxLength":1048576,"description":"complete UTF-8 file contents"}},"required":["path","content"],"additionalProperties":false}`,
	"list_worker_files":   `{"type":"object","properties":{"offset":{"type":"integer","minimum":0,"maximum":50000,"description":"zero-based result offset"},"limit":{"type":"integer","minimum":1,"maximum":200,"description":"maximum returned file entries"}},"additionalProperties":false}`,
	"search_worker_files": `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":4096,"pattern":"^[^\\r\\n]+$","description":"case-sensitive single-line literal text"},"offset":{"type":"integer","minimum":0,"maximum":10000,"description":"zero-based match offset"},"limit":{"type":"integer","minimum":1,"maximum":200,"description":"maximum returned matches"}},"required":["query"],"additionalProperties":false}`,
	"remove_worker_file":  `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096,"description":"file or internal symlink path relative to the worker"}},"required":["path"],"additionalProperties":false}`,
	"search_ouvrier_docs": `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
	"build_worker":        `{"type":"object","properties":{"target":{"type":"string","description":"GOOS/GOARCH, e.g. linux/amd64"}},"additionalProperties":false}`,
	"transfer_worker":     `{"type":"object","properties":{"env":{"type":"string","description":"deploy environment, e.g. staging"},"target":{"type":"string"},"env_file":{"type":"string"}},"required":["env"],"additionalProperties":false}`,
	"accept_risk":         `{"type":"object","properties":{"rationale":{"type":"string"}},"required":["rationale"],"additionalProperties":false}`,
}
