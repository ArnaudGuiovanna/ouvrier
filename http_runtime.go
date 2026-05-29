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
}

func defaultHTTPRuntime() httpRuntime {
	providers, _ := providerRegistryFromEnv()
	stream, _ := events.NewEventStream()
	return httpRuntime{
		providers:      providers,
		toolExecutor:   tools.NewExecutor(),
		mcpConnector:   envMCPConnector{connector: mcpclient.NewEnvConnector()},
		streamReceiver: newDefaultStreamReceiver(),
		streamDLQ:      newMemoryStreamDLQ(),
		eventStream:    stream,
		adminToken:     adminTokenFromEnv(),
		async:          newRuntimeAsyncGroup(),
	}
}

func defaultHTTPRuntimeForRun() (httpRuntime, func() error, error) {
	rt := defaultHTTPRuntime()
	store, err := state.NewStoreFromEnv()
	if err != nil {
		return httpRuntime{}, nil, err
	}
	rt.stateStore = store
	if err := seedHTTPEventStreamFromStore(&rt, store); err != nil {
		if closer, ok := store.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
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
	stream, err := events.NewEventStream(events.WithInitialID(maxID))
	if err != nil {
		return err
	}
	rt.eventStream = stream
	return nil
}

func (rt httpRuntime) runPlan(ctx context.Context, plan runtimeplan.Plan, input string) (string, error) {
	result, err := rt.runPlanResult(ctx, plan, input)
	return result.Output, err
}

func (rt httpRuntime) runPlanWithSession(ctx context.Context, plan runtimeplan.Plan, input string, session *runtimeplan.Session) (string, error) {
	result, err := rt.runPlanResultWithSession(ctx, plan, input, session)
	return result.Output, err
}

func (rt httpRuntime) runPlanResult(ctx context.Context, plan runtimeplan.Plan, input string) (planRunResult, error) {
	return rt.runPlanResultWithSession(ctx, plan, input, nil)
}

func (rt httpRuntime) runPlanResultWithSession(ctx context.Context, plan runtimeplan.Plan, input string, session *runtimeplan.Session) (planRunResult, error) {
	pipelineSession, err := pipelineSessionForPlan(plan, session)
	if err != nil {
		return planRunResult{Output: input}, err
	}
	pipelineResult := planRunResult{Output: input, Session: pipelineSession, HasSession: true}

	if err := rt.startPipelineExecution(ctx, pipelineSession, plan); err != nil {
		return pipelineResult, err
	}

	result, err := rt.runStepsResult(ctx, plan.Steps, input, planRunScope{parentSession: &pipelineSession})
	if !result.HasSession {
		result.Session = pipelineSession
		result.HasSession = true
	}
	if err != nil {
		emitErr := rt.finishPipelineExecution(ctx, pipelineSession, plan, "failed", err)
		return result, errors.Join(err, emitErr)
	}
	if err := rt.finishPipelineExecution(ctx, pipelineSession, plan, "completed", nil); err != nil {
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
	finishCtx := ctx
	if runtimeCancellationError(eventErr) {
		finishCtx = context.WithoutCancel(ctx)
	}
	kind := events.EventPipelineCompleted
	stateStatus := state.ExecutionCompleted
	if status != "completed" {
		kind = events.EventPipelineFailed
		stateStatus = state.ExecutionFailed
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
	if rt.stateStore == nil {
		return emitErr
	}
	return errors.Join(emitErr, rt.stateStore.SaveExecution(finishCtx, state.Execution{
		ExecID:      session.ExecID,
		TraceID:     session.TraceID,
		Status:      stateStatus,
		StartedAt:   session.StartedAt,
		CompletedAt: time.Now().UTC(),
	}))
}

func runtimeCancellationError(err error) bool {
	return errors.Is(err, context.Canceled)
}

type planRunScope struct {
	parentSession *runtimeplan.Session
	budgetLedger  *harness.BudgetLedger
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
	executor := tools.NewExecutor()
	if rt.toolExecutor != nil {
		executor = rt.toolExecutor.NewScope()
	}

	current := input
	for _, step := range steps {
		if step.Kind == runtimeplan.StepParallel {
			stepResult, err := rt.runParallelStepResult(ctx, step, current, scope)
			if err != nil {
				return stepResult, err
			}
			current = stepResult.Output
			result = stepResult
			continue
		}
		if step.Kind == runtimeplan.StepMap {
			stepResult, err := rt.runMapStepResult(ctx, step, current, scope)
			if err != nil {
				return stepResult, err
			}
			current = stepResult.Output
			result = stepResult
			continue
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
		out, err := h.Run(ctx, current)
		result.Session = out.Session
		result.HasSession = out.Session.SessionID != ""
		closeErr := closeMCP()
		if err != nil {
			result.Output = out.Text
			return result, err
		}
		if closeErr != nil {
			return result, closeErr
		}
		if out.Status != harness.StatusCompleted {
			result.Output = out.Text
			return result, fmt.Errorf("%w: %s", errHTTPPipelineIncomplete, out.Status)
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
