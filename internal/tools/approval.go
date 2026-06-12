package tools

import (
	"context"
	"fmt"
)

// ApprovalContext carries the execution identifiers needed to resume a
// suspended run once a gated tool call is approved.
type ApprovalContext struct {
	ExecID    string
	SessionID string
	TraceID   string
}

// ApprovalRequest is the redaction-safe description of a gated tool call that an
// ApprovalGate persists when an execution is suspended for human review. It
// never carries tool arguments or skill bodies.
type ApprovalRequest struct {
	ExecID     string
	SessionID  string
	TraceID    string
	ToolName   string
	ToolCallID string
	ToolKind   string
	Effect     string
	Reason     string
}

// ApprovalGate persists a pending approval and returns its identifier. It is the
// seam between the tool executor and the durable approval store.
type ApprovalGate interface {
	RecordPendingApproval(context.Context, ApprovalRequest) (string, error)
}

// SuspendedError halts a run when a gated tool call is recorded for human
// approval instead of being hard-denied. The HTTP runtime maps it to a 202
// Accepted with the approval and execution identifiers.
type SuspendedError struct {
	ApprovalID string
	ExecID     string
	ToolName   string
	ToolCallID string
}

func (e *SuspendedError) Error() string {
	return fmt.Sprintf("execution suspended for approval %q on tool %q", e.ApprovalID, e.ToolName)
}

type approvalGateContextValue struct {
	gate    ApprovalGate
	context ApprovalContext
}

type approvalGateContextKey struct{}

// ContextWithApprovalGate installs an approval gate plus the current execution
// context so the executor can suspend gated tool calls instead of denying them.
func ContextWithApprovalGate(ctx context.Context, gate ApprovalGate, approvalCtx ApprovalContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, approvalGateContextKey{}, approvalGateContextValue{gate: gate, context: approvalCtx})
}

func approvalGateFromContext(ctx context.Context) (ApprovalGate, ApprovalContext, bool) {
	if ctx == nil {
		return nil, ApprovalContext{}, false
	}
	value, ok := ctx.Value(approvalGateContextKey{}).(approvalGateContextValue)
	if !ok || value.gate == nil {
		return nil, ApprovalContext{}, false
	}
	return value.gate, value.context, true
}

type approvedApprovalContextValue struct {
	approvalID string
	toolCallID string
}

type approvedApprovalContextKey struct{}

// ContextWithApprovedApproval marks one previously suspended tool call as
// operator-approved for the duration of a resumed execution.
func ContextWithApprovedApproval(ctx context.Context, approvalID, toolCallID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if approvalID == "" || toolCallID == "" {
		return ctx
	}
	return context.WithValue(ctx, approvedApprovalContextKey{}, approvedApprovalContextValue{
		approvalID: approvalID,
		toolCallID: toolCallID,
	})
}

func approvedApprovalFromContext(ctx context.Context, toolCallID string) (string, bool) {
	if ctx == nil || toolCallID == "" {
		return "", false
	}
	value, ok := ctx.Value(approvedApprovalContextKey{}).(approvedApprovalContextValue)
	if !ok || value.toolCallID != toolCallID || value.approvalID == "" {
		return "", false
	}
	return value.approvalID, true
}
