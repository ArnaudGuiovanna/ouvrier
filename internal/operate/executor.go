package operate

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// GovernedExecutor is the single, unbypassable execution path for every
// governed cockpit action. One Execute call runs, in order and unskippably:
//
//  1. approval gate (honouring the Governance floor; fail-closed when headless)
//  2. sandbox-checked execution through the tool registry
//  3. redacted transcript persistence (tool_call + tool_result)
//  4. one JSONL tool-call audit record — on success, failure, and denial alike
//
// All cockpit surfaces (model tool loop, slash commands, IDE saves, operator
// shell) must execute governed operations through this interface; the raw
// registry dispatch is unexported and unreachable outside the executor.
type GovernedExecutor interface {
	Execute(ctx context.Context, call GovernedCall) (ToolResult, error)
}

// GovernedCall describes one governed Ouvrier operation.
type GovernedCall struct {
	// Session is the durable operate session receiving transcript and audit
	// records. It is required.
	Session *Session
	// ID is the stable tool_call id; generated when empty.
	ID string
	// Tool is the registry tool name to execute.
	Tool string
	// Input is the tool input, persisted (redacted) to the transcript.
	Input map[string]any
	// Posture is the operator's approval stance. It can only narrow what
	// auto-passes; the RequiresApproval governance floor always prompts.
	Posture Posture
	// Interactive marks an operator-attached call. Gated tools without an
	// interactive operator (or without Decisions) fail closed.
	Interactive bool
	// Decisions delivers the operator's answer to an emitted StreamApproval.
	Decisions <-chan ApprovalDecision
	// Emit optionally receives live StreamEvents (tool start/end, approval).
	Emit func(StreamEvent)
	// OnEntry optionally observes each persisted transcript entry.
	OnEntry func(TranscriptEntry)
}

// governedExecutor is the production GovernedExecutor bound to one runtime.
type governedExecutor struct {
	runtime *AgentRuntime
}

// Executor returns the runtime's governed executor.
func (r *AgentRuntime) Executor() GovernedExecutor {
	return governedExecutor{runtime: r}
}

// Execute runs one governed call. It returns ErrToolDenied when the approval
// gate blocks the tool; every outcome writes exactly one audit record.
func (e governedExecutor) Execute(ctx context.Context, call GovernedCall) (ToolResult, error) {
	r := e.runtime
	if r == nil || r.Tools == nil {
		return ToolResult{}, errors.New("operate: nil runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if call.Session == nil {
		return ToolResult{}, errors.New("operate: governed call requires a session")
	}
	if strings.TrimSpace(call.Tool) == "" {
		return ToolResult{}, errors.New("operate: governed call requires a tool name")
	}
	if call.Posture == "" {
		call.Posture = PostureManual
	}
	emit := call.Emit
	if emit == nil {
		emit = func(StreamEvent) {}
	}
	observe := call.OnEntry
	if observe == nil {
		observe = func(TranscriptEntry) {}
	}
	if call.ID == "" {
		id, err := randomID()
		if err != nil {
			return ToolResult{}, fmt.Errorf("operate: generate tool_call_id: %w", err)
		}
		call.ID = id
	}
	session := call.Session
	planned := plannedTool{ID: call.ID, Name: call.Tool, Input: call.Input}

	// Persist the tool_call before anything can run.
	callEntry, err := r.transcript(session).Append(TranscriptEntry{
		SessionID: session.ID,
		Kind:      TranscriptToolCall,
		ToolName:  call.Tool,
		Input:     call.Input,
		Metadata:  map[string]any{"tool_call_id": call.ID},
	})
	if err != nil {
		return ToolResult{}, err
	}
	observe(callEntry)
	emit(StreamEvent{Kind: StreamToolStart, Entry: &callEntry})

	// Approval gate. RequiresApproval always prompts; headless fails closed.
	if approved, gerr := e.gate(ctx, call, emit); gerr != nil || !approved {
		reason := "denied by operator"
		if gerr != nil {
			reason = gerr.Error()
		}
		result := ToolResult{Summary: "skipped " + call.Tool + ": " + reason}
		denyErr := fmt.Errorf("%s", reason)
		_ = appendToolCall(session.ToolCallsPath, r.Options.Redactor, planned, result, denyErr)
		output := map[string]any{"summary": result.Summary, "error": reason}
		resultEntry, aerr := r.transcript(session).Append(TranscriptEntry{
			SessionID: session.ID,
			Kind:      TranscriptToolResult,
			ToolName:  call.Tool,
			Output:    output,
			Metadata:  map[string]any{"tool_call_id": call.ID},
		})
		if aerr != nil {
			return ToolResult{}, aerr
		}
		observe(resultEntry)
		emit(StreamEvent{Kind: StreamToolEnd, Entry: &resultEntry, Err: denyErr})
		return result, ErrToolDenied
	}

	// Execute through the registry; each tool applies its own path sandbox.
	result, runErr := r.Tools.execute(ctx, ToolEnv{
		Harness:   r.Harness,
		Runtime:   r,
		Session:   session,
		Workspace: r.workspace,
		Options:   r.Options,
	}, call.Tool, call.Input)

	// One audit record per governed action, success or failure.
	_ = appendToolCall(session.ToolCallsPath, r.Options.Redactor, planned, result, runErr)

	output := result.Data
	if output == nil {
		output = map[string]any{}
	}
	output["summary"] = result.Summary
	if runErr != nil {
		output["error"] = runErr.Error()
	}
	resultEntry, err := r.transcript(session).Append(TranscriptEntry{
		SessionID: session.ID,
		Kind:      TranscriptToolResult,
		ToolName:  call.Tool,
		Output:    output,
		Metadata:  map[string]any{"tool_call_id": call.ID},
	})
	if err != nil {
		return ToolResult{}, err
	}
	observe(resultEntry)
	emit(StreamEvent{Kind: StreamToolEnd, Entry: &resultEntry, Err: runErr})
	return result, runErr
}

// gate blocks until the operator answers the approval request, or fails closed
// when the call is headless. Unknown tools pass through so the registry can
// report them as execution errors.
func (e governedExecutor) gate(ctx context.Context, call GovernedCall, emit func(StreamEvent)) (bool, error) {
	r := e.runtime
	tool, ok := r.Tools.Tool(call.Tool)
	if !ok {
		return true, nil
	}
	if !tool.Governance.NeedsApproval(call.Posture) {
		return true, nil
	}
	if !call.Interactive || call.Decisions == nil {
		return false, fmt.Errorf("approval required for %s but no operator is attached (headless)", call.Tool)
	}
	req := approvalRequestFor(plannedTool{ID: call.ID, Name: call.Tool, Input: call.Input}, tool.Governance, r.workspace)
	emit(StreamEvent{Kind: StreamApproval, Approval: req})
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case d := <-call.Decisions:
			if d.ID != "" && d.ID != req.ID {
				continue // ignore a stale/mismatched decision
			}
			return d.Approved, nil
		}
	}
}
