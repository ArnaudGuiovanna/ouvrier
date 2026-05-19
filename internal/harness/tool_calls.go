package harness

import (
	"context"
	"errors"
	"sync"

	"ouvrier/internal/events"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/tools"
)

type toolCallOutcome struct {
	result        provider.ToolResult
	toolErr       error
	budgetPayload map[string]any
}

func (h *Harness) executeToolCalls(ctx context.Context, session runtimecore.Session, calls []provider.ToolCall) ([]provider.Message, map[string]any, error) {
	messages := make([]provider.Message, 0, len(calls))
	for i := 0; i < len(calls); {
		if h.toolExecutor.CanRunParallelSubAgent(calls[i].Name) {
			start := i
			for i < len(calls) && h.toolExecutor.CanRunParallelSubAgent(calls[i].Name) {
				i++
			}
			groupMessages, payload, err := h.executeParallelToolCalls(ctx, session, calls[start:i])
			messages = append(messages, groupMessages...)
			if payload != nil || err != nil {
				return messages, payload, err
			}
			continue
		}

		message, payload, err := h.executeSingleToolCall(ctx, session, calls[i])
		if payload != nil || err != nil {
			return messages, payload, err
		}
		messages = append(messages, message)
		i++
	}
	return messages, nil, nil
}

func (h *Harness) executeParallelToolCalls(ctx context.Context, session runtimecore.Session, calls []provider.ToolCall) ([]provider.Message, map[string]any, error) {
	for _, call := range calls {
		if err := h.emitBeforeToolCall(ctx, session, call); err != nil {
			return nil, nil, err
		}
	}

	outcomes := make([]toolCallOutcome, len(calls))
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, call := range calls {
		i, call := i, call
		go func() {
			defer wg.Done()
			outcomes[i] = h.callTool(ctx, session, call)
		}()
	}
	wg.Wait()

	messages := make([]provider.Message, 0, len(calls))
	for i, outcome := range outcomes {
		message, payload, err := h.finishToolCall(ctx, session, calls[i], outcome)
		if payload != nil || err != nil {
			return messages, payload, err
		}
		messages = append(messages, message)
	}
	return messages, nil, nil
}

func (h *Harness) executeSingleToolCall(ctx context.Context, session runtimecore.Session, call provider.ToolCall) (provider.Message, map[string]any, error) {
	if err := h.emitBeforeToolCall(ctx, session, call); err != nil {
		return provider.Message{}, nil, err
	}
	return h.finishToolCall(ctx, session, call, h.callTool(ctx, session, call))
}

func (h *Harness) callTool(ctx context.Context, session runtimecore.Session, call provider.ToolCall) toolCallOutcome {
	toolCtx := contextWithExecution(ctx, session, h.budgetLedger)
	if h.stateStore != nil {
		toolCtx = tools.ContextWithIdempotencyStore(toolCtx, h.stateStore, session.ExecID)
	}
	toolCtx = tools.ContextWithPermissionDecisionObserver(toolCtx, func(ctx context.Context, audit tools.PermissionDecisionAudit) error {
		return h.emitPermissionDecision(ctx, session, audit)
	})
	result, err := h.toolExecutor.Execute(toolCtx, call)
	if err != nil {
		if payload, ok := h.wallClockBudgetPayload(ctx); ok {
			return toolCallOutcome{budgetPayload: payload}
		}
		return toolCallOutcome{toolErr: err}
	}
	return toolCallOutcome{result: result}
}

func (h *Harness) finishToolCall(ctx context.Context, session runtimecore.Session, call provider.ToolCall, outcome toolCallOutcome) (provider.Message, map[string]any, error) {
	if outcome.budgetPayload != nil {
		return provider.Message{}, outcome.budgetPayload, nil
	}
	if outcome.toolErr != nil {
		if emitErr := h.emit(ctx, session, events.EventAfterTool, map[string]any{
			"tool":  call.Name,
			"error": outcome.toolErr.Error(),
		}); emitErr != nil {
			return provider.Message{}, nil, errors.Join(outcome.toolErr, emitErr)
		}
		return provider.ToolResultText(call, outcome.toolErr.Error(), true), nil, nil
	}
	if err := h.emit(ctx, session, events.EventAfterTool, map[string]any{
		"tool": call.Name,
	}); err != nil {
		return provider.Message{}, nil, err
	}
	return provider.ToolResultMessage(outcome.result), nil, nil
}

func (h *Harness) emitBeforeToolCall(ctx context.Context, session runtimecore.Session, call provider.ToolCall) error {
	return h.emit(ctx, session, events.EventBeforeTool, map[string]any{
		"tool": call.Name,
	})
}

func (h *Harness) emitPermissionDecision(ctx context.Context, session runtimecore.Session, audit tools.PermissionDecisionAudit) error {
	allowed := audit.Decision.Allowed && audit.Err == nil
	payload := map[string]any{
		"action":            string(audit.Action.Kind),
		"tool":              audit.Action.ToolName,
		"tool_call_id":      audit.Action.ToolCallID,
		"allowed":           allowed,
		"effect":            string(normalizePermissionEffect(audit.Action.Effect)),
		"requires_approval": audit.Action.RequiresApproval,
	}
	if audit.Action.IdempotencyKey != "" {
		payload["idempotency_key_declared"] = true
	}
	if len(audit.Action.SideEffects) > 0 {
		payload["side_effects"] = append([]string(nil), audit.Action.SideEffects...)
	}
	if audit.Err != nil {
		payload["error"] = audit.Err.Error()
	} else if !audit.Decision.Allowed && audit.Decision.Reason != "" {
		payload["reason"] = audit.Decision.Reason
	}
	return h.emit(ctx, session, events.EventPermissionDecision, payload)
}

func normalizePermissionEffect(effect policy.Effect) policy.Effect {
	if effect == "" {
		return policy.EffectSideEffecting
	}
	return effect
}
