package tools

import (
	"context"
	"fmt"
	"strings"

	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
)

func (e *Executor) authorizeToolCall(ctx context.Context, tool registeredTool, call provider.ToolCall) (provider.ToolResult, bool, error) {
	permissionPolicy := e.policy
	if permissionPolicy == nil {
		permissionPolicy = policy.NewDefaultPolicy()
	}
	decision, err := permissionPolicy.Authorize(ctx, policy.Action{
		Kind:             policy.ActionToolCall,
		ToolName:         tool.name,
		ToolCallID:       call.ID,
		Effect:           normalizeEffect(tool.metadata.Effect),
		IdempotencyKey:   tool.metadata.IdempotencyKey,
		SideEffects:      append([]string(nil), tool.metadata.SideEffects...),
		RequiresApproval: tool.metadata.RequiresApproval,
	})
	if err != nil {
		return provider.ToolResult{}, false, err
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
