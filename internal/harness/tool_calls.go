package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

type toolCallOutcome struct {
	result        provider.ToolResult
	toolErr       error
	budgetPayload map[string]any
}

func (h *Harness) executeToolCalls(ctx context.Context, session runtimecore.Session, calls []provider.ToolCall) ([]provider.Message, map[string]any, error) {
	messages := make([]provider.Message, 0, len(calls))
	for i := 0; i < len(calls); {
		if !h.sequentialTools && h.toolExecutor.CanRunParallelSubAgent(calls[i].Name) {
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

func (h *Harness) allowsProviderRetryAfterToolCalls(calls []provider.ToolCall) bool {
	for _, call := range calls {
		if !h.toolExecutor.AllowsProviderRetryAfterToolCall(call.Name) {
			return false
		}
	}
	return true
}

func (h *Harness) executeParallelToolCalls(ctx context.Context, session runtimecore.Session, calls []provider.ToolCall) ([]provider.Message, map[string]any, error) {
	for _, call := range calls {
		if err := h.emitBeforeToolCall(ctx, session, call); err != nil {
			return nil, nil, err
		}
	}

	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	outcomes := make([]toolCallOutcome, len(calls))
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, call := range calls {
		i, call := i, call
		go func() {
			defer wg.Done()
			outcome := h.callTool(groupCtx, session, call)
			outcomes[i] = outcome
			if h.shouldCancelParallelToolCalls(call, outcome) {
				cancel()
			}
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

func (h *Harness) shouldCancelParallelToolCalls(call provider.ToolCall, outcome toolCallOutcome) bool {
	if outcome.budgetPayload != nil || outcome.toolErr != nil {
		return true
	}
	return outcome.result.IsError && h.subAgentFailureIsFatal(call.Name)
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
	toolCtx = tools.ContextWithToolRetry(toolCtx, h.providerRetries, h.retryBackoff)
	toolCtx = tools.ContextWithToolRetryObserver(toolCtx, func(ctx context.Context, audit tools.ToolRetryAudit) error {
		return h.emit(ctx, session, events.EventToolCallFailed, map[string]any{
			"tool":         audit.ToolName,
			"tool_call_id": audit.ToolCallID,
			"attempt":      audit.Attempt,
			"max_retries":  audit.MaxRetries,
			"effect":       string(audit.Effect),
			"error":        audit.Err.Error(),
			"transient":    provider.IsTransientError(audit.Err),
			"retrying":     true,
		})
	})
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
		if emitErr := h.emit(ctx, session, events.EventToolCallFailed, map[string]any{
			"tool":         call.Name,
			"tool_call_id": call.ID,
			"error":        outcome.toolErr.Error(),
		}); emitErr != nil {
			return provider.Message{}, nil, errors.Join(outcome.toolErr, emitErr)
		}
		if h.subAgentFailureIsFatal(call.Name) {
			if payload, ok := h.exceededBudgetPayload(); ok {
				return provider.Message{}, payload, nil
			}
			return provider.Message{}, nil, fmt.Errorf("subagent %q failed: %s", call.Name, outcome.toolErr.Error())
		}
		return provider.ToolResultText(call, outcome.toolErr.Error(), true), nil, nil
	}
	eventKind := events.EventAfterTool
	payload := map[string]any{
		"tool":         call.Name,
		"tool_call_id": call.ID,
	}
	if outcome.result.IsError {
		eventKind = events.EventToolCallFailed
		payload["error"] = "tool returned error result"
	}
	addToolResultObservability(payload, outcome.result.Content)
	if err := h.emit(ctx, session, eventKind, payload); err != nil {
		return provider.Message{}, nil, err
	}
	if outcome.result.IsError && h.subAgentFailureIsFatal(call.Name) {
		if payload, ok := h.exceededBudgetPayload(); ok {
			return provider.Message{}, payload, nil
		}
		return provider.Message{}, nil, fmt.Errorf("subagent %q failed: %s", call.Name, toolResultErrorText(outcome.result.Content))
	}
	return provider.ToolResultMessage(outcome.result), nil, nil
}

func (h *Harness) subAgentFailureIsFatal(name string) bool {
	return h.toolExecutor.CanRunParallelSubAgent(name) && !h.toolExecutor.SubAgentPartialOK(name)
}

func (h *Harness) exceededBudgetPayload() (map[string]any, bool) {
	if h.budgetLedger == nil {
		return nil, false
	}
	_, payload, exceeded := h.budgetLedger.Exceeded()
	return payload, exceeded
}

func toolResultErrorText(content json.RawMessage) string {
	var text string
	if err := json.Unmarshal(content, &text); err == nil && text != "" {
		return text
	}
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return "tool returned error result"
	}
	return string(content)
}

func addToolResultObservability(payload map[string]any, content json.RawMessage) {
	if payload == nil || len(bytes.TrimSpace(content)) == 0 {
		return
	}
	var fields map[string]any
	if err := json.Unmarshal(content, &fields); err != nil {
		return
	}
	if value, ok := fields["truncated"].(bool); ok && value {
		payload["output_truncated"] = true
	}
	if value, ok := fields["stdout_truncated"].(bool); ok && value {
		payload["stdout_truncated"] = true
	}
	if value, ok := fields["stderr_truncated"].(bool); ok && value {
		payload["stderr_truncated"] = true
	}
}

func (h *Harness) emitBeforeToolCall(ctx context.Context, session runtimecore.Session, call provider.ToolCall) error {
	return h.emit(ctx, session, events.EventBeforeTool, map[string]any{
		"tool":         call.Name,
		"tool_call_id": call.ID,
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
	if audit.Action.ToolKind != "" {
		payload["tool_kind"] = audit.Action.ToolKind
	}
	if audit.Action.Target != "" {
		payload["target"] = audit.Action.Target
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
