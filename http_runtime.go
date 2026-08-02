package ovr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/mcpclient"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/sandbox"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

var (
	errHTTPProviderNotConfigured = errors.New("http runtime provider not configured")
	errHTTPPipelineIncomplete    = errors.New("http runtime pipeline incomplete")
)

type httpPipelineIncompleteError struct {
	status harness.Status
	budget string
}

func (e *httpPipelineIncompleteError) Error() string {
	if e == nil {
		return errHTTPPipelineIncomplete.Error()
	}
	if e.budget == "" {
		return fmt.Sprintf("%s: %s", errHTTPPipelineIncomplete, e.status)
	}
	return fmt.Sprintf("%s: %s (budget=%s)", errHTTPPipelineIncomplete, e.status, e.budget)
}

func (e *httpPipelineIncompleteError) Unwrap() error {
	return errHTTPPipelineIncomplete
}

func newHTTPPipelineIncompleteError(out harness.Outcome) error {
	return &httpPipelineIncompleteError{
		status: out.Status,
		budget: out.BudgetExceeded,
	}
}

func httpPipelineBudget(err error) string {
	var incomplete *httpPipelineIncompleteError
	if !errors.As(err, &incomplete) || incomplete == nil {
		return ""
	}
	return incomplete.budget
}

type httpRuntime struct {
	provider             provider.Provider
	providers            *provider.Registry
	toolExecutor         *tools.Executor
	mcpConnector         mcpConnector
	streamReceiver       streamReceiver
	streamDLQ            streamDLQ
	stateStore           state.Store
	eventStream          *events.EventStream
	hookBus              *events.HookBus
	sandbox              *sandbox.Sandbox
	schemaRepairAttempts int
	pricing              provider.PricingTable
	adminToken           string
	adminRoutes          []httpRoute
	adminPlans           []adminPlanRoute
	async                *runtimeAsyncGroup
	streamDeltas         bool
	providerGate         *harness.ProviderGate
	approvalResumes      *approvalResumeRegistry
	// cronLease is set per fire by the leased cron loop so the fire's
	// pipeline events carry the leadership lease name, holder, and fence.
	cronLease *cronLeaseStamp
	// durableRuns enables the step-checkpoint run journal
	// (OUVRIER_DURABLE_RUNS=1); nil means off — zero journal writes.
	durableRuns *durableRunsConfig
}

func defaultHTTPRuntime() httpRuntime {
	providers, _ := providerRegistryFromEnv()
	stream, _ := events.NewEventStream()
	return httpRuntime{
		providers:       providers,
		toolExecutor:    tools.NewExecutor(),
		mcpConnector:    envMCPConnector{connector: mcpclient.NewEnvConnector()},
		streamReceiver:  newDefaultStreamReceiver(),
		streamDLQ:       newRoutingStreamDLQ(),
		eventStream:     stream,
		adminToken:      adminTokenFromEnv(),
		async:           newRuntimeAsyncGroup(),
		approvalResumes: newApprovalResumeRegistry(),
	}
}

func defaultHTTPRuntimeForRun() (httpRuntime, func() error, error) {
	rt := defaultHTTPRuntime()
	store, err := state.NewStoreFromEnv()
	if err != nil {
		return httpRuntime{}, nil, err
	}
	rt.stateStore = store
	closeStore := func() {
		if closer, ok := store.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	durable, err := durableRunsConfigForStore(store)
	if err != nil {
		closeStore()
		return httpRuntime{}, nil, err
	}
	rt.durableRuns = durable
	if err := seedHTTPEventStreamFromStore(&rt, store); err != nil {
		closeStore()
		return httpRuntime{}, nil, err
	}
	return rt, func() error {
		closer, ok := store.(interface{ Close() error })
		if !ok {
			return nil
		}
		return closer.Close()
	}, nil
}

func seedHTTPEventStreamFromStore(rt *httpRuntime, store state.Store) error {
	if rt == nil || rt.eventStream == nil || store == nil {
		return nil
	}
	recorded, err := store.EventsSince(context.Background(), "", 0)
	if err != nil {
		return err
	}
	var maxID uint64
	for _, event := range recorded {
		if event.ID > maxID {
			maxID = event.ID
		}
	}
	if maxID == 0 {
		return nil
	}
	rt.eventStream.EnsureNextIDAtLeast(maxID)
	return nil
}

func (rt httpRuntime) runPlanWithSession(ctx context.Context, plan runtimeplan.Plan, input string, session *runtimeplan.Session) (string, error) {
	result, err := rt.runPlanResultWithSession(ctx, plan, input, session)
	return result.Output, err
}

type planTerminalFunc func(context.Context, planRunResult) error

func (rt httpRuntime) runPlanResultWithTerminal(ctx context.Context, plan runtimeplan.Plan, input string, terminal planTerminalFunc) (planRunResult, error) {
	return rt.runPlanResultWithSessionAndTerminal(ctx, plan, input, nil, terminal)
}

func (rt httpRuntime) runPlanResultWithSession(ctx context.Context, plan runtimeplan.Plan, input string, session *runtimeplan.Session) (planRunResult, error) {
	return rt.runPlanResultWithSessionAndTerminal(ctx, plan, input, session, nil)
}

// runPlanResultWithSessionAndTerminal keeps the durable run lease and journal
// alive through the observable terminal. A run is completed, and its journal
// pruned, only after Reply validation or the Push/Sink output tool succeeds.
func (rt httpRuntime) runPlanResultWithSessionAndTerminal(ctx context.Context, plan runtimeplan.Plan, input string, session *runtimeplan.Session, terminal planTerminalFunc) (planRunResult, error) {
	pipelineSession, err := pipelineSessionForPlan(plan, session)
	if err != nil {
		return planRunResult{Output: input}, err
	}
	pipelineResult := planRunResult{Output: input, Session: pipelineSession, HasSession: true}

	if err := rt.startPipelineExecution(ctx, pipelineSession, plan); err != nil {
		return pipelineResult, errors.Join(err, rt.resolveReservedTriggerFailure(ctx, &pipelineSession))
	}

	scope := planRunScope{
		parentSession:        &pipelineSession,
		idempotencyNamespace: toolIdempotencyPlanNamespace(plan),
	}
	executionCtx := ctx
	var runLease *durableRunLeaseHeartbeat
	if rt.durableRuns != nil && rt.stateStore != nil {
		// Hold the run lease before the journal row exists so there is no
		// instant at which recovery could mistake this live run for an
		// orphan. The heartbeat renews at TTL/3 until the deferred release
		// after finishPipelineExecution (or after the run parks suspended).
		runLease, err = rt.acquireDurableRunLease(ctx, pipelineSession.ExecID)
		if err != nil {
			err = fmt.Errorf("durable run lease: %w", err)
			return pipelineResult, errors.Join(err, rt.finishPipelineExecution(ctx, pipelineSession, plan, "failed", err))
		}
		defer runLease.release()
		executionCtx = runLease.context()
		// Journal the run before its first step so a crash at any point
		// leaves a recoverable record. A failed journal write fails the run:
		// the operator opted in to durability, silently running without it
		// would be a lie.
		if err := rt.stateStore.SaveRunJournal(executionCtx, state.RunJournal{
			ExecID:      pipelineSession.ExecID,
			PlanKey:     durablePlanKey(plan),
			PlanHash:    durablePlanHash(plan),
			TriggerKind: string(plan.Trigger.Kind),
			Input:       input,
		}); err != nil {
			err = fmt.Errorf("durable run journal: %w", err)
			err = runLease.executionError(err)
			return pipelineResult, errors.Join(err, rt.finishPipelineExecution(executionCtx, pipelineSession, plan, "failed", err))
		}
		scope.durable = &durableStepJournal{store: rt.stateStore, execID: pipelineSession.ExecID}
	}

	result, err := rt.runStepsResult(executionCtx, plan.Steps, input, scope)
	if !result.HasSession {
		result.Session = pipelineSession
		result.HasSession = true
	}
	if err != nil {
		err = runLease.executionError(err)
		if !errors.Is(err, errDurableRunLeaseLost) && rt.rememberSuspendedPlan(err, plan, pipelineSession) {
			return result, err
		}
		emitErr := rt.finishPipelineExecution(executionCtx, pipelineSession, plan, "failed", err)
		return result, errors.Join(err, emitErr)
	}
	if runLease != nil {
		if err := runLease.confirm(executionCtx); err != nil {
			err = runLease.executionError(err)
			emitErr := rt.finishPipelineExecution(executionCtx, pipelineSession, plan, "failed", err)
			return result, errors.Join(err, emitErr)
		}
	}
	terminalEffectSucceeded := false
	if terminal != nil {
		terminalCtx := scope.durable.intentContext(executionCtx, len(plan.Steps))
		if err := terminal(terminalCtx, result); err != nil {
			err = runLease.executionError(err)
			emitErr := rt.finishPipelineExecution(executionCtx, pipelineSession, plan, "failed", err)
			return result, errors.Join(err, emitErr)
		}
		// The observable terminal effect is now confirmed. Persist its trigger
		// idempotency outcome immediately under a cancellation-independent,
		// bounded context so a disconnected HTTP client cannot reopen the key
		// and repeat a successful Push/Sink effect.
		settleCtx, settleCancel := newHTTPFinalizationContext(executionCtx)
		_ = rt.resolveTriggerIdempotency(settleCtx, pipelineSession.ExecID, "completed")
		settleCancel()
		terminalEffectSucceeded = true
	}
	if runLease != nil {
		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(executionCtx), cronLeaseReleaseTimeout)
		err := runLease.stopAndConfirm(confirmCtx)
		cancel()
		if err != nil {
			err = runLease.executionError(err)
			triggerOutcome := "failed"
			if terminalEffectSucceeded {
				// A terminal that already returned success must remain
				// idempotently succeeded even when the final lease proof fails.
				triggerOutcome = "completed"
			}
			emitErr := rt.finishPipelineExecutionWithTriggerOutcome(executionCtx, pipelineSession, plan, "failed", triggerOutcome, err)
			return result, errors.Join(err, emitErr)
		}
	}
	if err := rt.finishPipelineExecution(executionCtx, pipelineSession, plan, "completed", nil); err != nil {
		return result, err
	}
	return result, nil
}

func pipelineSessionForPlan(plan runtimeplan.Plan, session *runtimeplan.Session) (runtimeplan.Session, error) {
	if session != nil {
		return *session, nil
	}
	return newHTTPPipelineSession(plan)
}

func newHTTPPipelineSession(plan runtimeplan.Plan) (runtimeplan.Session, error) {
	return runtimeplan.NewSession(httpPipelineSessionModel(plan), runtimeplan.WithSessionBudget(runtimeplan.Budget{
		MaxIterations: harness.DefaultMaxIterations,
		MaxTokens:     harness.DefaultMaxTokens,
		MaxCostUSD:    harness.DefaultMaxCostUSD,
		MaxWallClock:  harness.DefaultMaxWallClock,
	}))
}

func httpPipelineSessionModel(plan runtimeplan.Plan) string {
	for _, step := range plan.Steps {
		if step.Model != "" {
			return step.Model
		}
	}
	return "runtime/http"
}

func (rt httpRuntime) startPipelineExecution(ctx context.Context, session runtimeplan.Session, plan runtimeplan.Plan) error {
	if rt.stateStore != nil {
		if err := rt.stateStore.SaveExecution(ctx, state.Execution{
			ExecID:    session.ExecID,
			TraceID:   session.TraceID,
			Status:    state.ExecutionRunning,
			StartedAt: session.StartedAt,
		}); err != nil {
			return err
		}
		if err := rt.stateStore.SaveSession(ctx, session); err != nil {
			return err
		}
	}
	if err := rt.emitSessionEvent(ctx, session, events.EventSessionStarted, map[string]any{
		"model": session.Model,
	}); err != nil {
		return errors.Join(err, rt.finishPipelineExecution(ctx, session, plan, "failed", err))
	}
	if err := rt.emitPipelineEvent(ctx, planRunResult{Session: session, HasSession: true}, plan, events.EventPipelineStarted, "started", nil); err != nil {
		return errors.Join(err, rt.finishPipelineExecution(ctx, session, plan, "failed", err))
	}
	return nil
}

func (rt httpRuntime) finishPipelineExecution(ctx context.Context, session runtimeplan.Session, plan runtimeplan.Plan, status string, eventErr error) error {
	return rt.finishPipelineExecutionWithTriggerOutcome(ctx, session, plan, status, status, eventErr)
}

func (rt httpRuntime) finishPipelineExecutionWithTriggerOutcome(ctx context.Context, session runtimeplan.Session, plan runtimeplan.Plan, status, triggerOutcome string, eventErr error) error {
	finishCtx, cancelFinish := newHTTPFinalizationContext(ctx)
	defer cancelFinish()
	kind := events.EventPipelineCompleted
	stateStatus := state.ExecutionCompleted
	if status != "completed" {
		kind = events.EventPipelineFailed
		stateStatus = state.ExecutionFailed
	}
	var idempotencyErr error
	if rt.stateStore != nil {
		// Resolve the trigger key before best-effort lifecycle observability. A
		// successful terminal must stay at-most-once even if saving the final
		// execution snapshot or an event fails afterward.
		idempotencyErr = rt.resolveTriggerIdempotency(finishCtx, session.ExecID, triggerOutcome)
	}
	var emitErr error
	if runtimeCancellationError(eventErr) {
		emitErr = errors.Join(emitErr, rt.emitSessionEvent(finishCtx, session, events.EventSessionCancelled, map[string]any{
			"status": status,
			"error":  eventErr.Error(),
		}))
	}
	emitErr = errors.Join(emitErr, rt.emitPipelineEvent(finishCtx, planRunResult{Session: session, HasSession: true}, plan, kind, status, eventErr))
	emitErr = errors.Join(emitErr, rt.emitSessionEvent(finishCtx, session, events.EventSessionSaved, map[string]any{
		"status": status,
	}))
	var saveErr error
	if rt.stateStore != nil {
		saveErr = rt.stateStore.SaveExecution(finishCtx, state.Execution{
			ExecID:      session.ExecID,
			TraceID:     session.TraceID,
			Status:      stateStatus,
			StartedAt:   session.StartedAt,
			CompletedAt: time.Now().UTC(),
		})
	}
	if rt.stateStore != nil && emitErr == nil && saveErr == nil && idempotencyErr == nil {
		// A successful run's recovery evidence is only disposable once terminal
		// events, final execution state, and trigger idempotency outcome are all
		// durable. Failed runs retain their journal by policy.
		rt.pruneDurableRunJournal(finishCtx, session, status)
	}
	return errors.Join(emitErr, saveErr, idempotencyErr)
}

// newHTTPFinalizationContext preserves request-scoped values but deliberately
// drops cancellation and the client deadline, then applies a short internal
// bound. Runtime state, idempotency, and journals therefore get a deterministic
// final write window without risking an unbounded shutdown.
func newHTTPFinalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(parent), cronLeaseReleaseTimeout)
}

// pruneDurableRunJournal applies durable-run retention when an execution
// finishes: a successful run's journal rows are pruned immediately, and the
// retention sweep removes terminal failed journals older than
// OUVRIER_DURABLE_RETENTION. Running, unknown, and pending-approval runs stay
// intact. Prune failures never fail the (already finished) run; they emit
// durable_run_prune_failed and surface in /admin/health.
func (rt httpRuntime) pruneDurableRunJournal(ctx context.Context, session runtimeplan.Session, status string) {
	if rt.durableRuns == nil || rt.stateStore == nil {
		return
	}
	if status == "completed" {
		if err := rt.stateStore.PruneRunJournal(ctx, session.ExecID); err != nil {
			rt.recordDurablePruneFailure(ctx, session, "completed_run", err)
		}
	}
	cutoff := time.Now().UTC().Add(-rt.durableRuns.retention)
	if _, err := rt.stateStore.PruneRunJournalsBefore(ctx, cutoff); err != nil {
		rt.recordDurablePruneFailure(ctx, session, "retention", err)
	}
}

func (rt httpRuntime) recordDurablePruneFailure(ctx context.Context, session runtimeplan.Session, scope string, err error) {
	rt.durableRuns.health.recordPruneFailure(err)
	_ = rt.emitSessionEvent(ctx, session, events.EventDurableRunPruneFailed, map[string]any{
		"scope": scope,
		"error": err.Error(),
	})
}

func runtimeCancellationError(err error) bool {
	return errors.Is(err, context.Canceled)
}

type planRunScope struct {
	parentSession *runtimeplan.Session
	budgetLedger  *harness.BudgetLedger
	// durable is non-nil only in the top-level step loop of a journaled run:
	// it checkpoints completed top-level steps and installs the tool intent
	// recorder. Parallel/Map strip it before running sub-branches (they
	// checkpoint as one unit) and subagents build their own scope without it.
	durable *durableStepJournal

	// idempotencyNamespace is a stable digest of the owning plan/Pipe
	// definition. idempotencyBase preserves absolute Pipe positions when a
	// suspended or durable run resumes with a sliced step list.
	idempotencyNamespace string
	idempotencyBase      int
}

type planRunResult struct {
	Output     string
	Session    runtimeplan.Session
	HasSession bool
}

func planRunResultFromInput(input string, session *runtimeplan.Session) planRunResult {
	result := planRunResult{Output: input}
	if session != nil {
		result.Session = *session
		result.HasSession = true
	}
	return result
}

func (rt httpRuntime) runSteps(ctx context.Context, steps []runtimeplan.Step, input string, scope planRunScope) (string, error) {
	result, err := rt.runStepsResult(ctx, steps, input, scope)
	return result.Output, err
}

func (rt httpRuntime) runStepsResult(ctx context.Context, steps []runtimeplan.Step, input string, scope planRunScope) (planRunResult, error) {
	result := planRunResult{Output: input}
	if len(steps) == 0 {
		return result, nil
	}

	current := input
	for stepIndex, step := range steps {
		stepScope := scope
		stepScope.idempotencyNamespace = toolIdempotencyStepNamespace(scope.idempotencyNamespace, scope.idempotencyBase+stepIndex, step)
		stepScope.idempotencyBase = 0
		// stepCtx carries the durable tool-intent recorder for this top-level
		// step; with durable runs off it is exactly ctx.
		stepCtx := scope.durable.intentContext(ctx, stepIndex)
		if step.Kind == runtimeplan.StepParallel {
			stepResult, err := rt.runParallelStepResult(stepCtx, step, current, stepScope)
			if err != nil {
				return stepResult, err
			}
			if err := scope.durable.checkpoint(ctx, stepIndex, stepResult.Output); err != nil {
				return stepResult, err
			}
			current = stepResult.Output
			result = stepResult
			continue
		}
		if step.Kind == runtimeplan.StepMap {
			stepResult, err := rt.runMapStepResult(stepCtx, step, current, stepScope)
			if err != nil {
				return stepResult, err
			}
			if err := scope.durable.checkpoint(ctx, stepIndex, stepResult.Output); err != nil {
				return stepResult, err
			}
			current = stepResult.Output
			result = stepResult
			continue
		}

		executor := tools.NewExecutor()
		if rt.toolExecutor != nil {
			executor = rt.toolExecutor.NewScope()
		}
		specs, closeMCP, err := rt.registerStepTools(ctx, executor, step)
		if err != nil {
			return result, err
		}
		stepProvider, err := rt.providerForModel(step.Model)
		if err != nil {
			_ = closeMCP()
			return result, err
		}
		systemPrompt, err := rt.systemPromptForStep(ctx, step, scope)
		if err != nil {
			_ = closeMCP()
			return result, err
		}
		harnessOptions := []harness.Option{
			harness.WithModel(step.Model),
			harness.WithSystemPrompt(systemPrompt),
			harness.WithToolExecutor(executor),
			harness.WithTools(specs...),
			harness.WithIdempotencyNamespace(stepScope.idempotencyNamespace),
		}
		if rt.stateStore != nil {
			harnessOptions = append(harnessOptions, harness.WithStateStore(rt.stateStore))
		}
		if rt.eventStream != nil {
			harnessOptions = append(harnessOptions, harness.WithEventStream(rt.eventStream))
		}
		if rt.hookBus != nil {
			harnessOptions = append(harnessOptions, harness.WithHookBus(rt.hookBus))
		}
		if step.ResultSchema != nil {
			harnessOptions = append(harnessOptions, harness.WithResultSchema(step.ResultSchema))
		}
		if runtimeBudgetConfigured(step.Budget) {
			harnessOptions = append(harnessOptions, harness.WithBudget(step.Budget))
		}
		if step.SequentialTools {
			harnessOptions = append(harnessOptions, harness.WithSequentialTools())
		}
		if rt.schemaRepairAttempts > 0 {
			harnessOptions = append(harnessOptions, harness.WithSchemaRepairAttempts(rt.schemaRepairAttempts))
		}
		if step.Retry != nil {
			harnessOptions = append(harnessOptions,
				harness.WithProviderRetries(step.Retry.ProviderRetries),
				harness.WithRetryBackoff(step.Retry.Backoff),
			)
		}
		if step.NoCache {
			harnessOptions = append(harnessOptions, harness.WithPromptCache(false))
		}
		if len(rt.pricing) > 0 {
			harnessOptions = append(harnessOptions, harness.WithPricing(rt.pricing))
		}
		if scope.parentSession != nil {
			harnessOptions = append(harnessOptions, harness.WithParentSession(*scope.parentSession))
		}
		if scope.budgetLedger != nil {
			harnessOptions = append(harnessOptions, harness.WithBudgetLedger(scope.budgetLedger))
		}
		if rt.streamDeltas {
			harnessOptions = append(harnessOptions, harness.WithStreaming(true))
		}
		if len(step.Fallback) > 0 {
			harnessOptions = append(harnessOptions,
				harness.WithFallback(step.Fallback...),
				harness.WithProviderResolver(rt.providerForModel),
			)
		}
		if rt.providerGate != nil {
			harnessOptions = append(harnessOptions, harness.WithProviderGate(rt.providerGate))
		}
		h, err := harness.New(stepProvider, harnessOptions...)
		if err != nil {
			_ = closeMCP()
			return result, err
		}
		out, err := h.Run(stepCtx, current)
		result.Session = out.Session
		result.HasSession = out.Session.SessionID != ""
		closeErr := closeMCP()
		if err != nil {
			result.Output = out.Text
			if suspended, ok := harness.SuspendedRun(err); ok {
				remainingSteps := append([]runtimeplan.Step(nil), steps[stepIndex+1:]...)
				suspendedStepIndex := stepIndex
				resume := func(resumeCtx context.Context) (planRunResult, error) {
					resumed, resumeErr := suspended.Resume(scope.durable.intentContext(resumeCtx, suspendedStepIndex))
					resumedResult := planRunResult{
						Output:     resumed.Text,
						Session:    resumed.Session,
						HasSession: resumed.Session.SessionID != "",
					}
					if resumeErr != nil {
						return resumedResult, resumeErr
					}
					if resumed.Status != harness.StatusCompleted {
						return resumedResult, newHTTPPipelineIncompleteError(resumed)
					}
					if err := scope.durable.checkpoint(resumeCtx, suspendedStepIndex, resumed.Text); err != nil {
						return resumedResult, err
					}
					if len(remainingSteps) == 0 {
						return resumedResult, nil
					}
					// Remaining steps run in a fresh local loop: offset the
					// checkpoint base so (exec_id, step_index) stays aligned
					// with the original plan.
					restScope := scope
					restScope.durable = scope.durable.withBase(suspendedStepIndex + 1)
					restScope.idempotencyBase = scope.idempotencyBase + suspendedStepIndex + 1
					restResult, restErr := rt.runStepsResult(resumeCtx, remainingSteps, resumed.Text, restScope)
					if !restResult.HasSession && resumedResult.HasSession {
						restResult.Session = resumedResult.Session
						restResult.HasSession = true
					}
					return restResult, restErr
				}
				return result, newSuspendedPlanError(suspended, resume)
			}
			return result, err
		}
		if closeErr != nil {
			return result, closeErr
		}
		if out.Status != harness.StatusCompleted {
			result.Output = out.Text
			return result, newHTTPPipelineIncompleteError(out)
		}
		if err := scope.durable.checkpoint(ctx, stepIndex, out.Text); err != nil {
			result.Output = out.Text
			return result, err
		}
		current = out.Text
		result.Output = current
	}
	return result, nil
}

func (rt httpRuntime) providerForModel(model string) (provider.Provider, error) {
	if rt.providers != nil {
		p, err := rt.providers.ForModel(model)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errHTTPProviderNotConfigured, err)
		}
		return p, nil
	}
	if rt.provider != nil {
		return rt.provider, nil
	}
	return nil, errHTTPProviderNotConfigured
}

type mcpConnector interface {
	Connect(context.Context, string) (mcpRuntimeSession, error)
}

type mcpRuntimeSession interface {
	RegisterTools(context.Context, *tools.Executor) ([]provider.ToolSpec, error)
	Close() error
}

func (rt httpRuntime) registerStepTools(ctx context.Context, executor *tools.Executor, step runtimeplan.Step) ([]provider.ToolSpec, func() error, error) {
	specs, err := registerRuntimeTools(executor, step.Tools)
	if err != nil {
		return nil, nil, err
	}
	bashSpecs, err := registerRuntimeBash(executor, step.Bash)
	if err != nil {
		return nil, nil, err
	}
	specs = append(specs, bashSpecs...)
	subAgentSpecs, err := registerRuntimeSubAgents(rt, executor, step.SubAgents)
	if err != nil {
		return nil, nil, err
	}
	specs = append(specs, subAgentSpecs...)

	sessions := make([]mcpRuntimeSession, 0, len(step.MCPServers))
	closeSessions := func() error {
		var closeErr error
		for _, session := range sessions {
			closeErr = errors.Join(closeErr, session.Close())
		}
		return closeErr
	}

	for _, server := range step.MCPServers {
		connector := rt.mcpConnector
		if connector == nil {
			connector = envMCPConnector{connector: mcpclient.NewEnvConnector()}
		}
		session, err := connector.Connect(ctx, server.Name)
		if err != nil {
			_ = closeSessions()
			return nil, nil, err
		}
		sessions = append(sessions, session)
		mcpSpecs, err := session.RegisterTools(ctx, executor)
		if err != nil {
			_ = closeSessions()
			return nil, nil, err
		}
		specs = append(specs, mcpSpecs...)
	}
	return specs, closeSessions, nil
}

type envMCPConnector struct {
	connector *mcpclient.EnvConnector
}

func (c envMCPConnector) Connect(ctx context.Context, serverName string) (mcpRuntimeSession, error) {
	return c.connector.Connect(ctx, serverName)
}

func registerRuntimeTools(executor *tools.Executor, runtimeTools []runtimeplan.Tool) ([]provider.ToolSpec, error) {
	specs := make([]provider.ToolSpec, 0, len(runtimeTools))
	for _, tool := range runtimeTools {
		if tool.GoFunc != nil {
			if err := executor.Register(tool.Name, tool.GoFunc, tools.WithMetadata(tools.Metadata{
				Effect:           tool.Effect,
				IdempotencyKey:   tool.IdempotencyKey,
				SideEffects:      tool.SideEffects,
				RequiresApproval: tool.RequiresApproval,
				ArgumentName:     tool.ArgumentName,
				InputSchema:      tool.InputSchema,
				Timeout:          tool.Timeout,
			})); err != nil {
				return nil, err
			}
		}
		specs = append(specs, provider.ToolSpec{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return specs, nil
}

func runtimeBudgetConfigured(budget runtimeplan.Budget) bool {
	return budget.MaxIterations > 0 ||
		budget.MaxTokens > 0 ||
		budget.MaxCostUSD > 0 ||
		budget.MaxWallClock > 0
}

type runtimeAsyncGroup struct {
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

func newRuntimeAsyncGroup() *runtimeAsyncGroup {
	ctx, cancel := context.WithCancel(context.Background())
	return &runtimeAsyncGroup{ctx: ctx, cancel: cancel}
}

func (g *runtimeAsyncGroup) Go(fn func(context.Context)) bool {
	if g == nil || fn == nil {
		return false
	}
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return false
	}
	ctx := g.ctx
	g.wg.Add(1)
	g.mu.Unlock()

	go func() {
		defer g.wg.Done()
		fn(ctx)
	}()
	return true
}

func (g *runtimeAsyncGroup) Shutdown(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if !g.stopped {
		g.stopped = true
		g.cancel()
	}
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()

	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rt httpRuntime) withAsyncGroup() httpRuntime {
	if rt.async == nil {
		rt.async = newRuntimeAsyncGroup()
	}
	if rt.approvalResumes == nil {
		rt.approvalResumes = newApprovalResumeRegistry()
	}
	if rt.stateStore != nil && rt.eventStream == nil {
		stream, err := events.NewEventStream()
		if err == nil {
			rt.eventStream = stream
			_ = rt.syncEventStreamWithStore(context.Background())
		}
	}
	return rt
}

func (rt httpRuntime) startAsync(fn func(context.Context)) bool {
	if rt.async == nil {
		rt.async = newRuntimeAsyncGroup()
	}
	return rt.async.Go(fn)
}

type runtimeHTTPHandler struct {
	handler http.Handler
	async   *runtimeAsyncGroup
}

func newRuntimeHTTPHandler(handler http.Handler, async *runtimeAsyncGroup) http.Handler {
	return &runtimeHTTPHandler{handler: handler, async: async}
}

func (h *runtimeHTTPHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.handler.ServeHTTP(w, req)
}

func (h *runtimeHTTPHandler) Shutdown(ctx context.Context) error {
	if h == nil || h.async == nil {
		return nil
	}
	return h.async.Shutdown(ctx)
}
