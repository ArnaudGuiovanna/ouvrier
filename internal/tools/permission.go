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
		if approvalID, ok := approvedApprovalFromContext(ctx, action.ToolCallID); ok {
			decision := policy.Decision{Allowed: true, ApprovalID: approvalID, Reason: "approval approved"}
			observeErr := observePermissionDecision(ctx, PermissionDecisionAudit{
				Action:   action,
				Decision: decision,
			})
			return provider.ToolResult{}, observeErr == nil, observeErr
		}
		// Durable-run recovery fallback (#40): a replayed run cannot match the
		// suspended tool call id (the provider re-mints it), so an installed
		// resolver matches the call by tool name + args hash against the
		// already-approved record instead of suspending a second time.
		if resolver, ok := approvedApprovalResolverFromContext(ctx); ok {
			if approvalID, ok := resolver(ctx, action.ToolName, toolIntentIdemKey(tool, call)); ok {
				decision := policy.Decision{Allowed: true, ApprovalID: approvalID, Reason: "approval approved before recovery"}
				observeErr := observePermissionDecision(ctx, PermissionDecisionAudit{
					Action:   action,
					Decision: decision,
				})
				return provider.ToolResult{}, observeErr == nil, observeErr
			}
		}
		if gate, approvalCtx, ok := approvalGateFromContext(ctx); ok {
			return e.suspendForApproval(ctx, tool, action, call, gate, approvalCtx)
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
	if decision.Suspended {
		approvalID := strings.TrimSpace(decision.ApprovalID)
		if approvalID == "" {
			reason := decision.Reason
			if strings.TrimSpace(reason) == "" {
				reason = "suspended permission decision requires an approval id"
			}
			return errorResult(call, fmt.Errorf("%w: %s", policy.ErrDenied, reason)), false, nil
		}
		_, approvalCtx, _ := approvalGateFromContext(ctx)
		return provider.ToolResult{}, false, &SuspendedError{
			ApprovalID: approvalID,
			ExecID:     approvalCtx.ExecID,
			ToolName:   action.ToolName,
			ToolCallID: action.ToolCallID,
		}
	}
	reason := decision.Reason
	if strings.TrimSpace(reason) == "" {
		reason = policy.ErrDenied.Error()
	}
	return errorResult(call, fmt.Errorf("%w: %s", policy.ErrDenied, reason)), false, nil
}

func (e *Executor) suspendForApproval(ctx context.Context, tool registeredTool, action policy.Action, call provider.ToolCall, gate ApprovalGate, approvalCtx ApprovalContext) (provider.ToolResult, bool, error) {
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
		// The args hash recorded at suspend time is what the recovery
		// resolver compares against on replay; both sides derive it from
		// toolIntentIdemKey so the digests match exactly.
		ArgsHash: toolIntentIdemKey(tool, call),
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
