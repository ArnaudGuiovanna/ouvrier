package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/schema"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

type Harness struct {
	provider         provider.Provider
	model            string
	systemPrompt     string
	budget           runtimecore.Budget
	budgetLedger     *BudgetLedger
	parentSession    *runtimecore.Session
	toolExecutor     *tools.Executor
	tools            []provider.ToolSpec
	stateStore       state.Store
	eventStream      *events.EventStream
	hookBus          *events.HookBus
	resultSchema     *runtimecore.ResultSchema
	schemaRepairs    int
	providerRetries  int
	retryBackoff     time.Duration
	promptCache      bool
	sequentialTools  bool
	pricing          provider.PricingTable
	streamDeltas     bool
	fallbackModels   []string
	providerResolver func(model string) (provider.Provider, error)
	providerGate     *ProviderGate
	memoryScope      string
	idempotencyScope string
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
		provider:         p,
		model:            cfg.model,
		systemPrompt:     cfg.systemPrompt,
		budget:           cfg.budget,
		budgetLedger:     ledger,
		parentSession:    cfg.parentSession,
		toolExecutor:     cfg.toolExecutor,
		tools:            append([]provider.ToolSpec(nil), cfg.tools...),
		stateStore:       cfg.stateStore,
		eventStream:      stream,
		hookBus:          cfg.hookBus,
		resultSchema:     cfg.resultSchema,
		schemaRepairs:    cfg.schemaRepairs,
		providerRetries:  cfg.providerRetries,
		retryBackoff:     cfg.retryBackoff,
		promptCache:      cfg.promptCache,
		sequentialTools:  cfg.sequentialTools,
		pricing:          cfg.pricing,
		streamDeltas:     cfg.streamDeltas,
		fallbackModels:   append([]string(nil), cfg.fallbackModels...),
		providerResolver: cfg.providerResolver,
		providerGate:     cfg.providerGate,
		memoryScope:      cfg.memoryScope,
		idempotencyScope: cfg.idempotencyScope,
	}, nil
}

// applyPricing overwrites resp.Usage.CostUSD when a configured pricing rate
// exists for the request model. When no table is configured or no rate
// matches, the existing best-effort cost is left untouched.
func (h *Harness) applyPricing(model string, resp *provider.Response) {
	if h == nil || resp == nil || len(h.pricing) == 0 {
		return
	}
	if cost, ok := h.pricing.Cost(model, resp.Usage, resp.Metadata.PromptCache); ok {
		resp.Usage.CostUSD = cost
	}
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

	return h.runLoop(runCtx, session, messages, out, providerRetryAllowed)
}

func (h *Harness) runLoop(runCtx context.Context, session runtimecore.Session, messages []provider.Message, out Outcome, providerRetryAllowed bool) (Outcome, error) {
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
		resp, err := h.completeWithFallback(runCtx, session, out.Iterations, provider.Request{
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
			"model":         h.model,
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
		out.Text = resp.Text
		if _, payload, exceeded := h.budgetLedger.Add(resp.Usage); exceeded {
			return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
		}
		if resp.StopReason == provider.StopMaxTokens {
			return h.truncateForBudget(
				context.WithoutCancel(runCtx),
				session,
				out,
				providerMaxTokensBudgetPayload(resp.Usage.OutputTokens),
			)
		}
		if len(resp.ToolCalls) == 0 {
			validated, repairUsage, err := h.validateResult(runCtx, session, out.Iterations, out.Text)
			if repairUsage.InputTokens != 0 || repairUsage.OutputTokens != 0 || repairUsage.CostUSD != 0 {
				out.Usage.Add(repairUsage)
				if _, payload, exceeded := h.budgetLedger.Add(repairUsage); exceeded {
					out.Text = validated
					return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
				}
			}
			out.Text = validated
			if payload, ok := providerMaxTokensErrorPayload(err); ok {
				return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
			}
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
			if suspended, ok := suspendedToolError(err); ok {
				baseMessages := append(cloneMessages(messages), toolMessages...)
				return h.suspendRun(runCtx, session, baseMessages, out, providerRetryAllowed, callsFromSuspended(resp.ToolCalls, suspended), suspended)
			}
			out.Status = StatusFailed
			return out, errors.Join(err, h.finishExecution(runCtx, session, out.Status, err))
		}
		messages = append(messages, toolMessages...)
		if _, payload, exceeded := h.budgetLedger.Exceeded(); exceeded {
			return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, payload)
		}
	}

	return h.truncateForBudget(context.WithoutCancel(runCtx), session, out, map[string]any{
		"budget":         "iterations",
		"max_iterations": h.budget.MaxIterations,
		"iterations":     out.Iterations,
	})
}

func (h *Harness) suspendRun(ctx context.Context, session runtimecore.Session, messages []provider.Message, out Outcome, providerRetryAllowed bool, calls []provider.ToolCall, suspended *tools.SuspendedError) (Outcome, error) {
	out.Status = StatusSuspended
	emitErr := h.emit(ctx, session, events.EventExecutionSuspended, map[string]any{
		"approval_id":  suspended.ApprovalID,
		"tool":         suspended.ToolName,
		"tool_call_id": suspended.ToolCallID,
	})
	resumeMessages := cloneMessages(messages)
	resumeCalls := append([]provider.ToolCall(nil), calls...)
	resumeOut := out
	resume := func(resumeCtx context.Context) (Outcome, error) {
		return h.resumeAfterApproval(resumeCtx, session, resumeMessages, resumeOut, providerRetryAllowed, resumeCalls, suspended)
	}
	return out, errors.Join(NewSuspendedRunError(suspended, resume), emitErr)
}

func (h *Harness) resumeAfterApproval(ctx context.Context, session runtimecore.Session, messages []provider.Message, out Outcome, providerRetryAllowed bool, calls []provider.ToolCall, suspended *tools.SuspendedError) (Outcome, error) {
	resumeCtx := ctx
	if resumeCtx == nil {
		resumeCtx = context.Background()
	}
	var cancel context.CancelFunc
	if h.budget.MaxWallClock > 0 {
		resumeCtx, cancel = context.WithTimeout(resumeCtx, h.budget.MaxWallClock)
		defer cancel()
	}

	approvedCtx := tools.ContextWithApprovedApproval(resumeCtx, suspended.ApprovalID, suspended.ToolCallID)
	toolMessages, budgetPayload, err := h.executeToolCalls(approvedCtx, session, calls)
	if budgetPayload != nil {
		return h.truncateForBudget(context.WithoutCancel(resumeCtx), session, out, budgetPayload)
	}
	if err != nil {
		if nextSuspended, ok := suspendedToolError(err); ok {
			baseMessages := append(cloneMessages(messages), toolMessages...)
			return h.suspendRun(resumeCtx, session, baseMessages, out, providerRetryAllowed, callsFromSuspended(calls, nextSuspended), nextSuspended)
		}
		out.Status = StatusFailed
		return out, errors.Join(err, h.finishExecution(resumeCtx, session, out.Status, err))
	}
	messages = append(cloneMessages(messages), toolMessages...)
	if _, payload, exceeded := h.budgetLedger.Exceeded(); exceeded {
		return h.truncateForBudget(context.WithoutCancel(resumeCtx), session, out, payload)
	}
	return h.runLoop(resumeCtx, session, messages, out, providerRetryAllowed)
}

func suspendedToolError(err error) (*tools.SuspendedError, bool) {
	var suspended *tools.SuspendedError
	if errors.As(err, &suspended) && suspended != nil {
		return suspended, true
	}
	return nil, false
}

func callsFromSuspended(calls []provider.ToolCall, suspended *tools.SuspendedError) []provider.ToolCall {
	if suspended == nil || suspended.ToolCallID == "" {
		return append([]provider.ToolCall(nil), calls...)
	}
	for i, call := range calls {
		if call.ID == suspended.ToolCallID {
			return append([]provider.ToolCall(nil), calls[i:]...)
		}
	}
	return append([]provider.ToolCall(nil), calls...)
}

func cloneMessages(messages []provider.Message) []provider.Message {
	return append([]provider.Message(nil), messages...)
}

// completeWithFallback tries the primary model and then each configured
// fallback model in order. A model is abandoned for the next candidate only
// when its call fails with a classified provider failure that fallback should
// react to (transient, rate-limit, or auth). Permanent and validation failures
// fail fast without falling through. Each candidate goes through the full
// provider retry loop before a fallback decision is made.
func (h *Harness) completeWithFallback(ctx context.Context, session runtimecore.Session, iteration int, req provider.Request, retryAllowed bool) (provider.Response, error) {
	models := append([]string{h.model}, h.fallbackModels...)
	var lastResp provider.Response
	var lastErr error
	for i, model := range models {
		p, resolveErr := h.providerForModel(model)
		if resolveErr != nil {
			lastResp, lastErr = provider.Response{}, resolveErr
			break
		}
		attempt := req
		attempt.Model = model
		resp, err := h.completeWithRetry(ctx, session, iteration, p, attempt, retryAllowed)
		if err == nil {
			return resp, nil
		}
		lastResp, lastErr = resp, err
		if i == len(models)-1 || !shouldFallback(err) {
			break
		}
		if emitErr := h.emitModelFallback(ctx, session, iteration, model, models[i+1], err); emitErr != nil {
			return provider.Response{}, errors.Join(err, emitErr)
		}
	}
	return lastResp, lastErr
}

func (h *Harness) providerForModel(model string) (provider.Provider, error) {
	if h.providerResolver != nil {
		return h.providerResolver(model)
	}
	return h.provider, nil
}

// shouldFallback reports whether a classified provider failure should trigger a
// fallback to the next model. Transient, rate-limit, and auth failures fall
// through; permanent and validation failures fail fast.
func shouldFallback(err error) bool {
	var classified provider.ClassifiedError
	if !errors.As(err, &classified) {
		return false
	}
	switch classified.Kind {
	case provider.ErrorTransient, provider.ErrorRateLimit, provider.ErrorAuth:
		return true
	default:
		return false
	}
}

func (h *Harness) completeWithRetry(ctx context.Context, session runtimecore.Session, iteration int, p provider.Provider, req provider.Request, retryAllowed bool) (provider.Response, error) {
	resp, err := h.complete(ctx, session, p, req)
	for attempt := 0; err != nil && retryAllowed && provider.IsTransientError(err) && attempt < h.providerRetries; attempt++ {
		if emitErr := h.emitProviderFailure(ctx, session, iteration, attempt+1, p, req.Model, err, true); emitErr != nil {
			return provider.Response{}, errors.Join(err, emitErr)
		}
		if waitErr := waitRetryBackoff(ctx, h.retryBackoff, attempt); waitErr != nil {
			return provider.Response{}, waitErr
		}
		resp, err = h.complete(ctx, session, p, req)
	}
	if err != nil {
		if emitErr := h.emitProviderFailure(ctx, session, iteration, providerAttemptNumber(err, retryAllowed, h.providerRetries), p, req.Model, err, false); emitErr != nil {
			return provider.Response{}, errors.Join(err, emitErr)
		}
		return resp, err
	}
	h.applyPricing(req.Model, &resp)
	return resp, err
}

// complete runs a single provider call. When streaming is enabled and the
// provider implements provider.StreamingProvider, it forwards each token delta
// to the event stream as an EventLLMTokenDelta event. The delta callback only
// appends to the in-memory event stream, so a slow SSE client draining the
// stream can never block or deadlock the harness. Providers without streaming
// support fall back to Complete.
func (h *Harness) complete(ctx context.Context, session runtimecore.Session, p provider.Provider, req provider.Request) (provider.Response, error) {
	release, err := h.acquireProviderSlot(ctx, p)
	if err != nil {
		return provider.Response{}, err
	}
	defer release()
	if h.streamDeltas {
		if streamer, ok := p.(provider.StreamingProvider); ok {
			index := 0
			redactor := events.NewTextStreamRedactor()
			emitDelta := func(text string) {
				if text == "" {
					return
				}
				_ = h.emit(ctx, session, events.EventLLMTokenDelta, map[string]any{
					"index": index,
					"text":  text,
				})
				index++
			}
			resp, streamErr := streamer.CompleteStream(ctx, req, func(delta provider.Delta) {
				emitDelta(redactor.Push(delta.Text))
			})
			// Flush even on provider failure: buffered ordinary text is still an
			// observable delta, while any incomplete credential state collapses
			// to a redaction marker.
			emitDelta(redactor.Flush())
			return resp, streamErr
		}
	}
	return p.Complete(ctx, req)
}

// acquireProviderSlot blocks on the per-provider concurrency gate, if any, so
// one provider's saturated rate budget cannot stall calls to other providers.
func (h *Harness) acquireProviderSlot(ctx context.Context, p provider.Provider) (func(), error) {
	if h.providerGate == nil || p == nil {
		return func() {}, nil
	}
	return h.providerGate.Acquire(ctx, p.Name())
}

func (h *Harness) emitProviderFailure(ctx context.Context, session runtimecore.Session, iteration, attempt int, p provider.Provider, model string, err error, retrying bool) error {
	payload := map[string]any{
		"iteration": iteration,
		"attempt":   attempt,
		"model":     model,
		"error":     err.Error(),
		"transient": provider.IsTransientError(err),
		"retrying":  retrying,
	}
	if p != nil {
		if name := strings.TrimSpace(p.Name()); name != "" {
			payload["provider"] = name
		}
	}
	if kind := providerErrorKind(err); kind != "" {
		payload["error_kind"] = string(kind)
	}
	return h.emit(ctx, session, events.EventLLMCallFailed, payload)
}

// emitModelFallback records a fallback decision on the trace. The payload is
// redaction-safe: it carries only model ids, the classified error kind, and a
// short error message (never request messages or skill bodies).
func (h *Harness) emitModelFallback(ctx context.Context, session runtimecore.Session, iteration int, fromModel, toModel string, err error) error {
	payload := map[string]any{
		"iteration":  iteration,
		"from_model": fromModel,
		"to_model":   toModel,
		"error":      err.Error(),
	}
	if kind := providerErrorKind(err); kind != "" {
		payload["error_kind"] = string(kind)
	}
	return h.emit(ctx, session, events.EventModelFallback, payload)
}

func providerErrorKind(err error) provider.ErrorKind {
	var classified provider.ClassifiedError
	if errors.As(err, &classified) {
		return classified.Kind
	}
	return ""
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
	if budget, ok := payload["budget"].(string); ok {
		out.BudgetExceeded = budget
	}
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
	normalized, err := schema.NormalizeJSON(h.resultSchema, []byte(text))
	if err != nil {
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
	return string(normalized), provider.Usage{}, h.emit(ctx, session, events.EventSchemaValidationPassed, map[string]any{
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
		resp, err := h.completeWithFallback(ctx, session, iteration, provider.Request{
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
			"model":         h.model,
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
		currentText = resp.Text
		if resp.StopReason == provider.StopMaxTokens {
			stopErr := &providerMaxTokensError{outputTokens: resp.Usage.OutputTokens}
			emitErr := h.emit(ctx, session, events.EventSchemaRepairFailed, map[string]any{
				"schema":        h.resultSchema.Name,
				"attempt":       attempt,
				"max_attempts":  h.schemaRepairs,
				"reason":        "provider_max_tokens",
				"stop_reason":   string(resp.StopReason),
				"output_tokens": resp.Usage.OutputTokens,
			})
			if emitErr != nil {
				return currentText, usage, emitErr
			}
			return currentText, usage, stopErr
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

		normalized, err := schema.NormalizeJSON(h.resultSchema, []byte(currentText))
		if err != nil {
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
		currentText = string(normalized)

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
		blocked := event
		updated, err := h.hookBus.Emit(ctx, event)
		if err != nil {
			return errors.Join(err, h.emitHookFailure(ctx, blocked, err))
		}
		event = updated
		event = events.SanitizeEvent(event)
	}
	return h.appendEvent(ctx, event)
}

func (h *Harness) emitHookFailure(ctx context.Context, blocked events.Event, err error) error {
	if err == nil || (h.eventStream == nil && h.stateStore == nil) {
		return nil
	}
	event := events.Event{
		Kind:      events.EventHookFailed,
		ExecID:    blocked.ExecID,
		SessionID: blocked.SessionID,
		TraceID:   blocked.TraceID,
		Payload: map[string]any{
			"blocked_kind": string(events.CanonicalKind(blocked.Kind)),
			"error":        err.Error(),
		},
	}
	return h.appendEvent(ctx, event)
}

func (h *Harness) appendEvent(ctx context.Context, event events.Event) error {
	event = events.SanitizeEvent(event)
	if h.eventStream == nil {
		if h.stateStore == nil {
			return nil
		}
		_, err := h.stateStore.AddEvent(ctx, event)
		return err
	}
	if h.stateStore == nil {
		_, err := h.eventStream.Append(ctx, event)
		return err
	}
	if _, globallyAllocated := h.stateStore.(state.GloballyAllocatedEventIDStore); globallyAllocated {
		_, err := h.eventStream.AppendPersisted(ctx, event, h.stateStore.AddEvent)
		return err
	}
	appended, err := h.eventStream.Append(ctx, event)
	if err != nil {
		return err
	}
	_, err = h.stateStore.AddEvent(ctx, appended)
	return err
}
