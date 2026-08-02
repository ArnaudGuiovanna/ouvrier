package operate

import (
	"context"
	"errors"
	"strings"
)

// StreamEventKind classifies one live cockpit event. Unlike the persisted
// transcript, these are designed to be consumed incrementally by a frontend
// (the Bubble Tea TUI) so the operator sees the agent work in real time instead
// of waiting for a whole turn to settle.
type StreamEventKind string

const (
	// StreamUser echoes the operator prompt as soon as it is recorded.
	StreamUser StreamEventKind = "user"
	// StreamAssistantDelta carries an incremental chunk of assistant text.
	StreamAssistantDelta StreamEventKind = "assistant_delta"
	// StreamAssistant carries the final persisted assistant entry.
	StreamAssistant StreamEventKind = "assistant"
	// StreamStatus carries a short non-persisted status line (e.g. the plan).
	StreamStatus StreamEventKind = "status"
	// StreamToolStart announces a tool call beginning.
	StreamToolStart StreamEventKind = "tool_start"
	// StreamToolEnd announces a tool call finishing with its result.
	StreamToolEnd StreamEventKind = "tool_end"
	// StreamError carries a turn-fatal error entry.
	StreamError StreamEventKind = "error"
	// StreamDone is the terminal event for one turn.
	StreamDone StreamEventKind = "done"
	// StreamApproval is emitted when a governed tool requires operator approval.
	StreamApproval StreamEventKind = "approval"
	// StreamReview is emitted after a successful review_worker tool call.
	StreamReview StreamEventKind = "review"
	// StreamDiff is emitted after a successful diff_worker tool call.
	StreamDiff StreamEventKind = "diff"
)

// ReviewData is the structured payload of a StreamReview event.
type ReviewData struct {
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// DiffData is the structured payload of a StreamDiff event.
type DiffData struct {
	Status       string   `json:"status"`
	ChangedFiles []string `json:"changed_files"`
	Patch        string   `json:"patch"`
}

// StreamEvent is one incremental cockpit event delivered over RunTurn's channel.
type StreamEvent struct {
	Kind      StreamEventKind  `json:"kind"`
	Entry     *TranscriptEntry `json:"entry,omitempty"`
	Delta     string           `json:"delta,omitempty"`
	Final     string           `json:"final,omitempty"`
	Outcome   RuntimeOutcome   `json:"outcome,omitempty"`
	Workspace *Workspace       `json:"workspace,omitempty"`
	Approval  *ApprovalRequest `json:"approval,omitempty"`
	Review    *ReviewData      `json:"review,omitempty"`
	Diff      *DiffData        `json:"diff,omitempty"`
	Err       error            `json:"-"`
}

// RunTurn executes one operator turn and streams live events over the returned
// channel, which is closed when the turn settles. The turn honours ctx
// cancellation between tool calls, which is how the TUI implements Esc-to-abort.
// It runs the exact same plan/tool path as Prompt, so print/json/rpc modes and
// the cockpit stay consistent.
func (r *AgentRuntime) RunTurn(ctx context.Context, sessionID, text, kind string) (<-chan StreamEvent, error) {
	if r == nil || r.Store == nil {
		return nil, errors.New("operate: nil runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(kind) == "" {
		kind = "prompt"
	}
	streamCtx, finishStream, err := r.beginRuntimeActivity(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamEvent, 32)
	go func() {
		defer finishStream()
		defer close(ch)
		done := false
		emit := func(ev StreamEvent) {
			ev = r.redactStreamEvent(ev)
			if ev.Kind == StreamDone {
				done = true
			}
			select {
			case <-streamCtx.Done():
			case ch <- ev:
			}
		}
		turn, runErr := r.runPrompt(streamCtx, sessionID, text, kind, emit, r.headlessControl())
		if runErr != nil && !done {
			emit(StreamEvent{Kind: StreamError, Err: runErr})
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Outcome: turn.Outcome, Workspace: turn.Workspace, Err: runErr})
		}
	}()
	return ch, nil
}

// RunTurnInteractive runs a turn with an operator approval channel. Send an
// ApprovalDecision (matching each emitted StreamApproval.ID) on the returned
// channel to unblock a governed tool.
func (r *AgentRuntime) RunTurnInteractive(ctx context.Context, sessionID, text, kind string, posture Posture) (<-chan StreamEvent, chan<- ApprovalDecision, error) {
	if r == nil || r.Store == nil {
		return nil, nil, errors.New("operate: nil runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(kind) == "" {
		kind = "prompt"
	}
	if posture == "" {
		posture = PostureManual
	}
	streamCtx, finishStream, err := r.beginRuntimeActivity(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan StreamEvent, 32)
	decisions := make(chan ApprovalDecision, 1)
	ctrl := &turnControl{posture: posture, decisions: decisions, interactive: true}
	go func() {
		defer finishStream()
		defer close(ch)
		done := false
		emit := func(ev StreamEvent) {
			ev = r.redactStreamEvent(ev)
			if ev.Kind == StreamDone {
				done = true
			}
			select {
			case <-streamCtx.Done():
			case ch <- ev:
			}
		}
		turn, runErr := r.runPrompt(streamCtx, sessionID, text, kind, emit, ctrl)
		if runErr != nil && !done {
			emit(StreamEvent{Kind: StreamError, Err: runErr})
			emit(StreamEvent{Kind: StreamDone, Final: turn.Final, Outcome: turn.Outcome, Workspace: turn.Workspace, Err: runErr})
		}
	}()
	return ch, decisions, nil
}

// emitAssistantDeltas chunks a finished assistant message into word-sized deltas
// so the frontend can render a typing effect even when the underlying driver
// returns text in one shot (e.g. the manual planner). Real streaming drivers can
// emit StreamAssistantDelta directly.
func emitAssistantDeltas(emit func(StreamEvent), text string) {
	if emit == nil || strings.TrimSpace(text) == "" {
		return
	}
	const chunk = 24
	runes := []rune(text)
	for i := 0; i < len(runes); i += chunk {
		end := min(i+chunk, len(runes))
		emit(StreamEvent{Kind: StreamAssistantDelta, Delta: string(runes[i:end])})
	}
}
