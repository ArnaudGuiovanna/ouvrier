package harness

import (
	"context"
	"errors"
	"sync"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
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
	result, err := h.toolExecutor.Execute(contextWithExecution(ctx, session, h.budgetLedger), call)
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
