package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	schemaRepairs   int
	providerRetries int
	retryBackoff    time.Duration
	promptCache     bool
	sequentialTools bool
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
		schemaRepairs:   cfg.schemaRepairs,
		providerRetries: cfg.providerRetries,
		retryBackoff:    cfg.retryBackoff,
		promptCache:     cfg.promptCache,
		sequentialTools: cfg.sequentialTools,
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
	providerRetryAllowed := true
	if err := h.startExecution(runCtx, session); err != nil {
		out.Status = StatusFailed
		return out, err
	}

	for out.Iterations < h.budget.MaxIterations {
		if _, payload, exceeded := h.budgetLedger.Exceeded(); exceeded {
			return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
		}
		out.Iterations++
		if err := h.emit(runCtx, session, events.EventBeforeLLM, map[string]any{
			"iteration": out.Iterations,
			"model":     h.model,
		}); err != nil {
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status, err))
		}
		resp, err := h.completeWithRetry(runCtx, session, out.Iterations, provider.Request{
			Model:    h.model,
			System:   h.requestSystemPrompt(),
			Messages: append([]provider.Message(nil), messages...),
			Tools:    append([]provider.ToolSpec(nil), h.tools...),
			CacheKey: h.requestCacheKey(),
		}, providerRetryAllowed)
		if err != nil {
			if payload, ok := h.wallClockBudgetPayload(runCtx); ok {
				return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
			}
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status, err))
		}

		out.Usage.Add(resp.Usage)
		afterLLM := map[string]any{
			"iteration":     out.Iterations,
			"stop_reason":   string(resp.StopReason),
			"tool_calls":    len(resp.ToolCalls),
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"cost_usd":      resp.Usage.CostUSD,
		}
		addLLMResponseMetadata(afterLLM, resp.Metadata)
		if err := h.emit(runCtx, session, events.EventAfterLLM, afterLLM); err != nil {
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status, err))
		}
		if resp.Text != "" {
			out.Text = resp.Text
		}
		if _, payload, exceeded := h.budgetLedger.Add(resp.Usage); exceeded {
			return h.truncateForBudget(runCtx, session, out, payload)
		}
		if len(resp.ToolCalls) == 0 {
			validated, repairUsage, err := h.validateResult(runCtx, session, out.Iterations, out.Text)
			if repairUsage.InputTokens != 0 || repairUsage.OutputTokens != 0 || repairUsage.CostUSD != 0 {
				out.Usage.Add(repairUsage)
				if _, payload, exceeded := h.budgetLedger.Add(repairUsage); exceeded {
					out.Text = validated
					return h.truncateForBudget(runCtx, session, out, payload)
				}
			}
			out.Text = validated
			if err != nil {
				out.Status = StatusFailed
				return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status, err))
			}
			out.Status = StatusCompleted
			return out, h.finishExecution(runCtx, session, out.Status)
		}

		out.ToolCalls = append(out.ToolCalls, resp.ToolCalls...)
		messages = append(messages, provider.AssistantToolCalls(resp.Text, resp.ToolCalls...))
		if !h.allowsProviderRetryAfterToolCalls(resp.ToolCalls) {
			providerRetryAllowed = false
		}
		toolMessages, budgetPayload, err := h.executeToolCalls(runCtx, session, resp.ToolCalls)
		if budgetPayload != nil {
			return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, budgetPayload)
		}
		if err != nil {
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status, err))
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

func (h *Harness) completeWithRetry(ctx context.Context, session runtimecore.Session, iteration int, req provider.Request, retryAllowed bool) (provider.Response, error) {
	resp, err := h.provider.Complete(ctx, req)
	for attempt := 0; err != nil && retryAllowed && provider.IsTransientError(err) && attempt < h.providerRetries; attempt++ {
		if emitErr := h.emitProviderFailure(ctx, session, iteration, attempt+1, req.Model, err, true); emitErr != nil {
			return provider.Response{}, errors.Join(err, emitErr)
		}
		if waitErr := waitRetryBackoff(ctx, h.retryBackoff, attempt); waitErr != nil {
			return provider.Response{}, waitErr
		}
		resp, err = h.provider.Complete(ctx, req)
	}
	if err != nil {
		if emitErr := h.emitProviderFailure(ctx, session, iteration, providerAttemptNumber(err, retryAllowed, h.providerRetries), req.Model, err, false); emitErr != nil {
			return provider.Response{}, errors.Join(err, emitErr)
		}
	}
	return resp, err
}

func (h *Harness) emitProviderFailure(ctx context.Context, session runtimecore.Session, iteration, attempt int, model string, err error, retrying bool) error {
	return h.emit(ctx, session, events.EventLLMCallFailed, map[string]any{
		"iteration": iteration,
		"attempt":   attempt,
		"model":     model,
		"error":     err.Error(),
		"transient": provider.IsTransientError(err),
		"retrying":  retrying,
	})
}

func addLLMResponseMetadata(payload map[string]any, metadata provider.ResponseMetadata) {
	if payload == nil {
		return
	}
	if metadata.Provider != "" {
		payload["provider"] = metadata.Provider
	}
	if metadata.Model != "" {
		payload["provider_model"] = metadata.Model
	}
	if metadata.Latency > 0 {
		payload["latency_ms"] = metadata.Latency.Milliseconds()
	}
	addPromptCacheMetadata(payload, metadata.PromptCache)
}

func addPromptCacheMetadata(payload map[string]any, metadata provider.PromptCacheMetadata) {
	if payload == nil || !metadata.Requested {
		return
	}
	payload["prompt_cache_requested"] = true
	payload["prompt_cache_supported"] = metadata.Supported
	payload["prompt_cache_applied"] = metadata.Applied
	if metadata.ReadInputTokens > 0 {
		payload["prompt_cache_read_tokens"] = metadata.ReadInputTokens
	}
	if metadata.WriteInputTokens > 0 {
		payload["prompt_cache_write_tokens"] = metadata.WriteInputTokens
	}
	if metadata.Reason != "" {
		payload["prompt_cache_reason"] = metadata.Reason
	}
}

func providerAttemptNumber(err error, retryAllowed bool, maxRetries int) int {
	if retryAllowed && provider.IsTransientError(err) {
		return maxRetries + 1
	}
	return 1
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

func (h *Harness) requestSystemPrompt() string {
	prompt := strings.TrimSpace(h.systemPrompt)
	if h.resultSchema == nil {
		return prompt
	}
	schemaPrompt := fmt.Sprintf(`Return only valid JSON that conforms to the required result schema.

Schema name: %s
JSON Schema:
%s

Do not include markdown, prose, or tool calls in the final answer.`, h.resultSchema.Name, string(h.resultSchema.JSONSchema))
	if prompt == "" {
		return schemaPrompt
	}
	return prompt + "\n\n" + schemaPrompt
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
		if err := h.emit(ctx, session, events.EventSessionStart, map[string]any{
			"model": h.model,
		}); err != nil {
			return err
		}
		return h.emit(ctx, session, events.EventPipeStarted, map[string]any{
			"model": h.model,
		})
	}
	if h.stateStore == nil {
		if err := h.emit(ctx, session, events.EventSessionStart, map[string]any{
			"model": h.model,
		}); err != nil {
			return err
		}
		return h.emit(ctx, session, events.EventPipeStarted, map[string]any{
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
	if err := h.emit(ctx, session, events.EventSessionStart, map[string]any{
		"model": h.model,
	}); err != nil {
		return err
	}
	return h.emit(ctx, session, events.EventPipeStarted, map[string]any{
		"model": h.model,
	})
}

func (h *Harness) finishExecution(ctx context.Context, session runtimecore.Session, status Status, eventErr ...error) error {
	finishCtx := ctx
	if isCancellationError(errors.Join(eventErr...)) {
		finishCtx = context.WithoutCancel(ctx)
	}
	pipeKind := events.EventPipeFailed
	if status == StatusCompleted {
		pipeKind = events.EventPipeCompleted
	}
	payload := map[string]any{
		"model":  h.model,
		"status": string(status),
	}
	if len(eventErr) > 0 && eventErr[0] != nil {
		payload["error"] = eventErr[0].Error()
	}
	var emitErr error
	if isCancellationError(errors.Join(eventErr...)) {
		emitErr = errors.Join(emitErr, h.emit(finishCtx, session, events.EventSessionCancelled, map[string]any{
			"status": string(status),
			"error":  errors.Join(eventErr...).Error(),
		}))
	}
	emitErr = errors.Join(emitErr, h.emit(finishCtx, session, pipeKind, payload))
	emitErr = errors.Join(emitErr, h.emit(finishCtx, session, events.EventSessionEnd, map[string]any{
		"status": string(status),
	}))
	if h.stateStore == nil || h.parentSession != nil {
		return emitErr
	}
	return errors.Join(emitErr, h.stateStore.SaveExecution(finishCtx, state.Execution{
		ExecID:      session.ExecID,
		TraceID:     session.TraceID,
		Status:      executionStatus(status),
		StartedAt:   session.StartedAt,
		CompletedAt: time.Now().UTC(),
	}))
}

func isCancellationError(err error) bool {
	return errors.Is(err, context.Canceled)
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

func (h *Harness) validateResult(ctx context.Context, session runtimecore.Session, iteration int, text string) (string, provider.Usage, error) {
	if h.resultSchema == nil {
		return text, provider.Usage{}, nil
	}
	if err := schema.ValidateJSON(h.resultSchema, []byte(text)); err != nil {
		recordErr := h.recordSchemaViolation(ctx, session, err)
		if recordErr != nil {
			return text, provider.Usage{}, errors.Join(err, recordErr)
		}
		if h.schemaRepairs <= 0 {
			emitErr := h.emit(ctx, session, events.EventSchemaRepairFailed, map[string]any{
				"schema":       h.resultSchema.Name,
				"attempt":      0,
				"max_attempts": 0,
				"reason":       "disabled",
				"error":        err.Error(),
			})
			return text, provider.Usage{}, errors.Join(err, emitErr)
		}
		return h.repairResult(ctx, session, iteration, text, err)
	}
	return text, provider.Usage{}, h.emit(ctx, session, events.EventSchemaValidationPassed, map[string]any{
		"schema": h.resultSchema.Name,
	})
}

func (h *Harness) repairResult(ctx context.Context, session runtimecore.Session, iteration int, text string, validationErr error) (string, provider.Usage, error) {
	var usage provider.Usage
	currentText := text
	currentErr := validationErr

	for attempt := 1; attempt <= h.schemaRepairs; attempt++ {
		if err := h.emit(ctx, session, events.EventSchemaRepairStarted, map[string]any{
			"schema":       h.resultSchema.Name,
			"attempt":      attempt,
			"max_attempts": h.schemaRepairs,
			"error":        currentErr.Error(),
		}); err != nil {
			return currentText, usage, err
		}

		if err := h.emit(ctx, session, events.EventBeforeLLM, map[string]any{
			"iteration": iteration,
			"model":     h.model,
			"repair":    true,
			"attempt":   attempt,
		}); err != nil {
			return currentText, usage, err
		}
		resp, err := h.completeWithRetry(ctx, session, iteration, provider.Request{
			Model:    h.model,
			System:   h.systemPrompt,
			CacheKey: h.requestCacheKeyFor(h.systemPrompt, nil),
			Messages: []provider.Message{
				provider.UserText(schemaRepairPrompt(h.resultSchema, currentText, currentErr)),
			},
		}, true)
		if err != nil {
			emitErr := h.emit(ctx, session, events.EventSchemaRepairFailed, map[string]any{
				"schema":       h.resultSchema.Name,
				"attempt":      attempt,
				"max_attempts": h.schemaRepairs,
				"error":        err.Error(),
			})
			return currentText, usage, errors.Join(err, emitErr)
		}
		usage.Add(resp.Usage)
		afterLLM := map[string]any{
			"iteration":     iteration,
			"stop_reason":   string(resp.StopReason),
			"tool_calls":    len(resp.ToolCalls),
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"cost_usd":      resp.Usage.CostUSD,
			"repair":        true,
			"attempt":       attempt,
		}
		addLLMResponseMetadata(afterLLM, resp.Metadata)
		if err := h.emit(ctx, session, events.EventAfterLLM, afterLLM); err != nil {
			return currentText, usage, err
		}
		if len(resp.ToolCalls) > 0 {
			err := errors.New("schema repair returned tool calls")
			emitErr := h.emit(ctx, session, events.EventSchemaRepairFailed, map[string]any{
				"schema":       h.resultSchema.Name,
				"attempt":      attempt,
				"max_attempts": h.schemaRepairs,
				"error":        err.Error(),
			})
			return currentText, usage, errors.Join(err, emitErr)
		}

		currentText = resp.Text
		if err := schema.ValidateJSON(h.resultSchema, []byte(currentText)); err != nil {
			currentErr = err
			recordErr := h.recordSchemaViolation(ctx, session, err)
			emitErr := h.emit(ctx, session, events.EventSchemaRepairFailed, map[string]any{
				"schema":       h.resultSchema.Name,
				"attempt":      attempt,
				"max_attempts": h.schemaRepairs,
				"error":        err.Error(),
			})
			if recordErr != nil || emitErr != nil {
				return currentText, usage, errors.Join(err, recordErr, emitErr)
			}
			continue
		}

		if err := h.emit(ctx, session, events.EventSchemaRepairCompleted, map[string]any{
			"schema":       h.resultSchema.Name,
			"attempt":      attempt,
			"max_attempts": h.schemaRepairs,
		}); err != nil {
			return currentText, usage, err
		}
		if err := h.emit(ctx, session, events.EventSchemaValidationPassed, map[string]any{
			"schema":          h.resultSchema.Name,
			"repaired":        true,
			"repair_attempts": attempt,
		}); err != nil {
			return currentText, usage, err
		}
		return currentText, usage, nil
	}

	return currentText, usage, currentErr
}

func schemaRepairPrompt(contract *runtimecore.ResultSchema, output string, validationErr error) string {
	return fmt.Sprintf(`The previous assistant output did not match the required result schema.

Schema name: %s
Validation error: %s
JSON Schema:
%s

Invalid output:
%s

Return only valid JSON that conforms to the schema. Do not include markdown, prose, or tool calls.`, contract.Name, validationErr.Error(), string(contract.JSONSchema), output)
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
	event = events.SanitizeEvent(event)
	if h.hookBus != nil {
		var err error
		event, err = h.hookBus.Emit(ctx, event)
		if err != nil {
			return err
		}
		event = events.SanitizeEvent(event)
	}
	if h.eventStream == nil {
		if h.stateStore == nil {
			return nil
		}
		_, err := h.stateStore.AddEvent(ctx, event)
		return err
	}
	appended, err := h.eventStream.Append(ctx, event)
	if err != nil {
		return err
	}
	if h.stateStore == nil {
		return nil
	}
	_, err = h.stateStore.AddEvent(ctx, appended)
	return err
}
