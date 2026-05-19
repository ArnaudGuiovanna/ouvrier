package harness

import (
	"context"
	"errors"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/schema"
	"ouvrier/internal/state"
	"ouvrier/internal/tools"
)

type Harness struct {
	provider        provider.Provider
	model           string
	systemPrompt    string
	budget          runtimecore.Budget
	budgetLedger    *BudgetLedger
	parentSession   *runtimecore.Session
	toolExecutor    *tools.Executor
	tools           []provider.ToolSpec
	stateStore      state.Store
	eventStream     *events.EventStream
	hookBus         *events.HookBus
	resultSchema    *runtimecore.ResultSchema
	providerRetries int
	retryBackoff    time.Duration
}

func New(p provider.Provider, opts ...Option) (*Harness, error) {
	if p == nil {
		return nil, errors.New("provider is required")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	if _, err := provider.ParseModelID(cfg.model); err != nil {
		return nil, err
	}
	stream := cfg.eventStream
	if stream == nil {
		var err error
		stream, err = events.NewEventStream()
		if err != nil {
			return nil, err
		}
	}
	ledger := cfg.budgetLedger
	if ledger == nil {
		ledger = NewBudgetLedger(cfg.budget)
	}
	return &Harness{
		provider:        p,
		model:           cfg.model,
		systemPrompt:    cfg.systemPrompt,
		budget:          cfg.budget,
		budgetLedger:    ledger,
		parentSession:   cfg.parentSession,
		toolExecutor:    cfg.toolExecutor,
		tools:           append([]provider.ToolSpec(nil), cfg.tools...),
		stateStore:      cfg.stateStore,
		eventStream:     stream,
		hookBus:         cfg.hookBus,
		resultSchema:    cfg.resultSchema,
		providerRetries: cfg.providerRetries,
		retryBackoff:    cfg.retryBackoff,
	}, nil
}

func (h *Harness) Run(ctx context.Context, input string) (Outcome, error) {
	runCtx := ctx
	if runCtx == nil {
		runCtx = context.Background()
	}
	var cancel context.CancelFunc
	if h.budget.MaxWallClock > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, h.budget.MaxWallClock)
		defer cancel()
	}

	session, err := h.newSession()
	if err != nil {
		return Outcome{Status: StatusFailed}, err
	}

	messages := []provider.Message{provider.UserText(input)}
	out := Outcome{Session: session}
	toolAttempted := false
	if err := h.startExecution(runCtx, session); err != nil {
		out.Status = StatusFailed
		return out, err
	}

	for out.Iterations < h.budget.MaxIterations {
		out.Iterations++
		if err := h.emit(runCtx, session, events.EventBeforeLLM, map[string]any{
			"iteration": out.Iterations,
			"model":     h.model,
		}); err != nil {
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status))
		}
		resp, err := h.completeWithRetry(runCtx, provider.Request{
			Model:    h.model,
			System:   h.systemPrompt,
			Messages: append([]provider.Message(nil), messages...),
			Tools:    append([]provider.ToolSpec(nil), h.tools...),
		}, !toolAttempted)
		if err != nil {
			if payload, ok := h.wallClockBudgetPayload(runCtx); ok {
				return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
			}
			out.Status = StatusFailed
			emitErr := h.emit(runCtx, session, events.EventAfterLLM, map[string]any{
				"iteration": out.Iterations,
				"error":     err.Error(),
			})
			return out, errors.Join(err, emitErr, h.finishExecution(runCtx, session, out.Status))
		}

		out.Usage.Add(resp.Usage)
		if err := h.emit(runCtx, session, events.EventAfterLLM, map[string]any{
			"iteration":     out.Iterations,
			"stop_reason":   string(resp.StopReason),
			"tool_calls":    len(resp.ToolCalls),
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"cost_usd":      resp.Usage.CostUSD,
		}); err != nil {
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status))
		}
		if resp.Text != "" {
			out.Text = resp.Text
		}
		if _, payload, exceeded := h.budgetLedger.Add(resp.Usage); exceeded {
			return h.truncateForBudget(runCtx, session, out, payload)
		}
		if len(resp.ToolCalls) == 0 {
			if err := h.validateResult(runCtx, session, out.Text); err != nil {
				out.Status = StatusFailed
				return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status))
			}
			out.Status = StatusCompleted
			return out, h.finishExecution(runCtx, session, out.Status)
		}

		out.ToolCalls = append(out.ToolCalls, resp.ToolCalls...)
		messages = append(messages, provider.AssistantToolCalls(resp.Text, resp.ToolCalls...))
		toolAttempted = true
		toolMessages, budgetPayload, err := h.executeToolCalls(runCtx, session, resp.ToolCalls)
		if budgetPayload != nil {
			return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, budgetPayload)
		}
		if err != nil {
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status))
		}
		messages = append(messages, toolMessages...)
		if _, payload, exceeded := h.budgetLedger.Exceeded(); exceeded {
			return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
		}
	}

	return h.truncateForBudget(runCtx, session, out, map[string]any{
		"budget":         "iterations",
		"max_iterations": h.budget.MaxIterations,
		"iterations":     out.Iterations,
	})
}

func (h *Harness) completeWithRetry(ctx context.Context, req provider.Request, retryAllowed bool) (provider.Response, error) {
	resp, err := h.provider.Complete(ctx, req)
	for attempt := 0; err != nil && retryAllowed && provider.IsTransientError(err) && attempt < h.providerRetries; attempt++ {
		if waitErr := waitRetryBackoff(ctx, h.retryBackoff, attempt); waitErr != nil {
			return provider.Response{}, waitErr
		}
		resp, err = h.provider.Complete(ctx, req)
	}
	return resp, err
}

func waitRetryBackoff(ctx context.Context, backoff time.Duration, attempt int) error {
	if backoff <= 0 {
		return nil
	}
	delay := backoff * time.Duration(1<<attempt)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Harness) newSession() (runtimecore.Session, error) {
	opts := []runtimecore.SessionOption{runtimecore.WithSessionBudget(h.budget)}
	if h.parentSession != nil {
		return runtimecore.NewChildSession(*h.parentSession, h.model, opts...)
	}
	return runtimecore.NewSession(h.model, opts...)
}

func (h *Harness) startExecution(ctx context.Context, session runtimecore.Session) error {
	if h.parentSession != nil {
		if h.stateStore != nil {
			if err := h.stateStore.SaveSession(ctx, session); err != nil {
				return err
			}
		}
		return h.emit(ctx, session, events.EventSessionStart, map[string]any{
			"model": h.model,
		})
	}
	if h.stateStore == nil {
		return h.emit(ctx, session, events.EventSessionStart, map[string]any{
			"model": h.model,
		})
	}
	execution := state.Execution{
		ExecID:    session.ExecID,
		TraceID:   session.TraceID,
		Status:    state.ExecutionRunning,
		StartedAt: session.StartedAt,
	}
	if err := h.stateStore.SaveExecution(ctx, execution); err != nil {
		return err
	}
	if err := h.stateStore.SaveSession(ctx, session); err != nil {
		markErr := h.finishExecution(ctx, session, StatusFailed)
		return errors.Join(err, markErr)
	}
	return h.emit(ctx, session, events.EventSessionStart, map[string]any{
		"model": h.model,
	})
}

func (h *Harness) finishExecution(ctx context.Context, session runtimecore.Session, status Status) error {
	emitErr := h.emit(ctx, session, events.EventSessionEnd, map[string]any{
		"status": string(status),
	})
	if h.stateStore == nil || h.parentSession != nil {
		return emitErr
	}
	return errors.Join(emitErr, h.stateStore.SaveExecution(ctx, state.Execution{
		ExecID:      session.ExecID,
		TraceID:     session.TraceID,
		Status:      executionStatus(status),
		StartedAt:   session.StartedAt,
		CompletedAt: time.Now().UTC(),
	}))
}

func executionStatus(status Status) state.ExecutionStatus {
	switch status {
	case StatusCompleted:
		return state.ExecutionCompleted
	case StatusTruncated:
		return state.ExecutionTruncated
	default:
		return state.ExecutionFailed
	}
}

func (h *Harness) wallClockBudgetPayload(ctx context.Context) (map[string]any, bool) {
	if h.budget.MaxWallClock <= 0 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, false
	}
	return map[string]any{
		"budget":           "wallclock",
		"max_wallclock_ms": h.budget.MaxWallClock.Milliseconds(),
	}, true
}

func (h *Harness) truncateForBudget(ctx context.Context, session runtimecore.Session, out Outcome, payload map[string]any) (Outcome, error) {
	out.Status = StatusTruncated
	if err := h.emit(ctx, session, events.EventBudgetExceeded, payload); err != nil {
		out.Status = StatusFailed
		return out, errors.Join(err, h.finishExecution(ctx, session, out.Status))
	}
	return out, h.finishExecution(ctx, session, out.Status)
}

func (h *Harness) validateResult(ctx context.Context, session runtimecore.Session, text string) error {
	if h.resultSchema == nil {
		return nil
	}
	if err := schema.ValidateJSON(h.resultSchema, []byte(text)); err != nil {
		recordErr := h.recordSchemaViolation(ctx, session, err)
		return errors.Join(err, recordErr)
	}
	return h.emit(ctx, session, events.EventSchemaValidationPassed, map[string]any{
		"schema": h.resultSchema.Name,
	})
}

func (h *Harness) recordSchemaViolation(ctx context.Context, session runtimecore.Session, validationErr error) error {
	violation := state.SchemaViolation{
		ExecID:     session.ExecID,
		SessionID:  session.SessionID,
		SchemaName: h.resultSchema.Name,
		Error:      validationErr.Error(),
	}
	var err error
	if h.stateStore != nil {
		_, err = h.stateStore.AddSchemaViolation(ctx, violation)
	}
	emitErr := h.emit(ctx, session, events.EventSchemaViolation, map[string]any{
		"schema": h.resultSchema.Name,
		"error":  validationErr.Error(),
	})
	return errors.Join(err, emitErr)
}

func (h *Harness) emit(ctx context.Context, session runtimecore.Session, kind events.EventKind, payload map[string]any) error {
	if h.eventStream == nil && h.hookBus == nil {
		return nil
	}
	event := events.Event{
		Kind:      kind,
		ExecID:    session.ExecID,
		SessionID: session.SessionID,
		TraceID:   session.TraceID,
		Payload:   payload,
	}
	if h.hookBus != nil {
		var err error
		event, err = h.hookBus.Emit(ctx, event)
		if err != nil {
			return err
		}
	}
	if h.eventStream == nil {
		return nil
	}
	_, err := h.eventStream.Append(ctx, event)
	return err
}
