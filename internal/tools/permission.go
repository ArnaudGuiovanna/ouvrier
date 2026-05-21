package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
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
	action := policy.Action{
		Kind:             policy.ActionToolCall,
		ToolName:         tool.name,
		ToolCallID:       call.ID,
		ToolKind:         string(tool.metadata.Kind),
		Effect:           normalizeEffect(tool.metadata.Effect),
		IdempotencyKey:   tool.metadata.IdempotencyKey,
		SideEffects:      append([]string(nil), tool.metadata.SideEffects...),
		RequiresApproval: tool.metadata.RequiresApproval,
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
