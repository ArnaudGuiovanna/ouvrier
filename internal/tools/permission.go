package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type PermissionDecisionAudit struct {
	Action   policy.Action
	Decision policy.Decision
	Err      error
}

type PermissionDecisionObserver func(context.Context, PermissionDecisionAudit) error

type permissionDecisionObserverContextKey struct{}

func ContextWithPermissionDecisionObserver(ctx context.Context, observer PermissionDecisionObserver) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, permissionDecisionObserverContextKey{}, observer)
}

func (e *Executor) authorizeToolCall(ctx context.Context, tool registeredTool, call provider.ToolCall) (provider.ToolResult, bool, error) {
	permissionPolicy := e.policy
	if permissionPolicy == nil {
		permissionPolicy = policy.NewDefaultPolicy()
	}
	actionKind := tool.metadata.ActionKind
	if actionKind == "" {
		actionKind = policy.ActionToolCall
	}
	action := policy.Action{
		Kind:             actionKind,
		ToolName:         tool.name,
		ToolCallID:       call.ID,
		ToolKind:         string(tool.metadata.Kind),
		Target:           tool.metadata.Target,
		Effect:           normalizeEffect(tool.metadata.Effect),
		IdempotencyKey:   tool.metadata.IdempotencyKey,
		SideEffects:      append([]string(nil), tool.metadata.SideEffects...),
		RequiresApproval: tool.metadata.RequiresApproval,
	}
	if action.RequiresApproval {
		if gate, approvalCtx, ok := approvalGateFromContext(ctx); ok {
			return e.suspendForApproval(ctx, action, call, gate, approvalCtx)
		}
	}
	decision, err := permissionPolicy.Authorize(ctx, action)
	observeErr := observePermissionDecision(ctx, PermissionDecisionAudit{
		Action:   action,
		Decision: decision,
		Err:      err,
	})
	if err != nil {
		return provider.ToolResult{}, false, errors.Join(err, observeErr)
	}
	if observeErr != nil {
		return provider.ToolResult{}, false, observeErr
	}
	if decision.Allowed {
		return provider.ToolResult{}, true, nil
	}
	reason := decision.Reason
	if strings.TrimSpace(reason) == "" {
		reason = policy.ErrDenied.Error()
	}
	return errorResult(call, fmt.Errorf("%w: %s", policy.ErrDenied, reason)), false, nil
}

func (e *Executor) suspendForApproval(ctx context.Context, action policy.Action, call provider.ToolCall, gate ApprovalGate, approvalCtx ApprovalContext) (provider.ToolResult, bool, error) {
	reason := "tool requires explicit approval"
	approvalID, recordErr := gate.RecordPendingApproval(ctx, ApprovalRequest{
		ExecID:     approvalCtx.ExecID,
		SessionID:  approvalCtx.SessionID,
		TraceID:    approvalCtx.TraceID,
		ToolName:   action.ToolName,
		ToolCallID: action.ToolCallID,
		ToolKind:   action.ToolKind,
		Effect:     string(action.Effect),
		Reason:     reason,
	})
	decision := policy.Decision{Allowed: false, Suspended: true, ApprovalID: approvalID, Reason: reason}
	if recordErr != nil {
		decision = policy.Decision{Allowed: false, Reason: reason}
	}
	observeErr := observePermissionDecision(ctx, PermissionDecisionAudit{
		Action:   action,
		Decision: decision,
		Err:      recordErr,
	})
	if recordErr != nil {
		return provider.ToolResult{}, false, errors.Join(recordErr, observeErr)
	}
	if observeErr != nil {
		return provider.ToolResult{}, false, observeErr
	}
	return provider.ToolResult{}, false, &SuspendedError{
		ApprovalID: approvalID,
		ExecID:     approvalCtx.ExecID,
		ToolName:   action.ToolName,
		ToolCallID: action.ToolCallID,
	}
}

func observePermissionDecision(ctx context.Context, audit PermissionDecisionAudit) error {
	if ctx == nil {
		return nil
	}
	observer, ok := ctx.Value(permissionDecisionObserverContextKey{}).(PermissionDecisionObserver)
	if !ok || observer == nil {
		return nil
	}
	return observer(ctx, audit)
}
