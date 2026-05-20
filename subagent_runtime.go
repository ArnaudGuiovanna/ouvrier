package ovr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ouvrier/internal/events"
	"ouvrier/internal/harness"
	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
	runtimeplan "ouvrier/internal/runtime"
	"ouvrier/internal/tools"
)

var subAgentInputSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"additionalProperties":true}`)

type subAgentHandler struct {
	runtime  httpRuntime
	spec     runtimeplan.SubAgent
	parallel chan struct{}
}

func registerRuntimeSubAgents(rt httpRuntime, executor *tools.Executor, subAgents []runtimeplan.SubAgent) ([]provider.ToolSpec, error) {
	specs := make([]provider.ToolSpec, 0, len(subAgents))
	for _, subAgent := range subAgents {
		handler := newSubAgentHandler(rt, subAgent)
		if err := executor.RegisterHandler(subAgent.Name, handler, tools.WithMetadata(tools.Metadata{
			Effect: policy.EffectSideEffecting,
			Kind:   tools.ToolKindSubAgent,
		})); err != nil {
			return nil, err
		}
		specs = append(specs, provider.ToolSpec{
			Name:        subAgent.Name,
			Description: fmt.Sprintf("Run the %q subagent pipeline.", subAgent.Name),
			InputSchema: append(json.RawMessage(nil), subAgentInputSchema...),
		})
	}
	return specs, nil
}

func newSubAgentHandler(rt httpRuntime, spec runtimeplan.SubAgent) *subAgentHandler {
	maxParallel := spec.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 1
	}
	return &subAgentHandler{
		runtime:  rt,
		spec:     spec,
		parallel: make(chan struct{}, maxParallel),
	}
}

func (h *subAgentHandler) Execute(ctx context.Context, call provider.ToolCall) (provider.ToolResult, error) {
	parent, ok := harness.SessionFromContext(ctx)
	if !ok {
		return provider.ToolResult{}, errors.New("subagent requires parent session context")
	}
	input, err := subAgentInput(call.Arguments)
	if err != nil {
		return provider.ToolResult{}, err
	}
	if err := h.acquire(ctx); err != nil {
		return provider.ToolResult{}, err
	}
	defer h.release()

	if err := h.emitTask(ctx, parent, events.EventTaskStarted, call, nil); err != nil {
		return provider.ToolResult{}, err
	}
	scope := planRunScope{parentSession: &parent}
	if ledger, ok := harness.BudgetLedgerFromContext(ctx); ok {
		scope.budgetLedger = ledger
	}
	output, err := h.runtime.runSteps(ctx, h.spec.Pipeline.Steps, input, scope)
	if err != nil {
		emitErr := h.emitTask(ctx, parent, events.EventTaskFailed, call, map[string]any{"error": err.Error()})
		return provider.ToolResult{}, errors.Join(err, emitErr)
	}
	if err := h.emitTask(ctx, parent, events.EventTaskCompleted, call, nil); err != nil {
		return provider.ToolResult{}, err
	}

	content := json.RawMessage(output)
	if !json.Valid(content) {
		content, err = json.Marshal(output)
		if err != nil {
			return provider.ToolResult{}, fmt.Errorf("marshal subagent result: %w", err)
		}
	}
	return provider.ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    content,
	}, nil
}

func (h *subAgentHandler) acquire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case h.parallel <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *subAgentHandler) release() {
	<-h.parallel
}

func (h *subAgentHandler) emitTask(ctx context.Context, parent runtimeplan.Session, kind events.EventKind, call provider.ToolCall, extra map[string]any) error {
	if h.runtime.eventStream == nil && h.runtime.hookBus == nil {
		return nil
	}
	payload := map[string]any{
		"subagent":     h.spec.Name,
		"tool_call_id": call.ID,
		"max_parallel": h.spec.MaxParallel,
	}
	for key, value := range extra {
		payload[key] = value
	}
	return h.runtime.emitSessionEvent(ctx, parent, kind, payload)
}

func subAgentInput(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var input string
		if err := json.Unmarshal(raw, &input); err != nil {
			return "", fmt.Errorf("decode subagent input: %w", err)
		}
		return input, nil
	}

	var args struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("decode subagent input: %w", err)
	}
	if args.Input != "" {
		return args.Input, nil
	}
	return string(raw), nil
}
