package ovr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/harness"
	"ouvrier/internal/mcpclient"
	"ouvrier/internal/provider"
	runtimeplan "ouvrier/internal/runtime"
	"ouvrier/internal/state"
	"ouvrier/internal/tools"
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
	stateStore           state.Store
	eventStream          *events.EventStream
	hookBus              *events.HookBus
	schemaRepairAttempts int
	adminToken           string
	adminRoutes          []httpRoute
}

func defaultHTTPRuntime() httpRuntime {
	providers, _ := providerRegistryFromEnv()
	stream, _ := events.NewEventStream()
	return httpRuntime{
		providers:    providers,
		toolExecutor: tools.NewExecutor(),
		mcpConnector: envMCPConnector{connector: mcpclient.NewEnvConnector()},
		eventStream:  stream,
		adminToken:   adminTokenFromEnv(),
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

func (rt httpRuntime) runPlanResult(ctx context.Context, plan runtimeplan.Plan, input string) (planRunResult, error) {
	pipelineSession, err := newHTTPPipelineSession(plan)
	if err != nil {
		return planRunResult{Output: input}, err
	}
	pipelineResult := planRunResult{Output: input, Session: pipelineSession, HasSession: true}

	if err := rt.startPipelineExecution(ctx, pipelineSession, plan); err != nil {
		return pipelineResult, err
	}

	result, err := rt.runStepsResult(ctx, plan.Steps, input, planRunScope{parentSession: &pipelineSession})
	if err != nil {
		if !result.HasSession {
			result.Session = pipelineSession
			result.HasSession = true
		}
		emitErr := rt.finishPipelineExecution(ctx, pipelineSession, plan, "failed", err)
		return result, errors.Join(err, emitErr)
	}
	if err := rt.finishPipelineExecution(ctx, pipelineSession, plan, "completed", nil); err != nil {
		return result, err
	}
	return result, nil
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
	if err := rt.emitPipelineEvent(ctx, planRunResult{Session: session, HasSession: true}, plan, events.EventPipelineStarted, "started", nil); err != nil {
		return errors.Join(err, rt.finishPipelineExecution(ctx, session, plan, "failed", err))
	}
	return nil
}

func (rt httpRuntime) finishPipelineExecution(ctx context.Context, session runtimeplan.Session, plan runtimeplan.Plan, status string, eventErr error) error {
	kind := events.EventPipelineCompleted
	stateStatus := state.ExecutionCompleted
	if status != "completed" {
		kind = events.EventPipelineFailed
		stateStatus = state.ExecutionFailed
	}
	emitErr := rt.emitPipelineEvent(ctx, planRunResult{Session: session, HasSession: true}, plan, kind, status, eventErr)
	if rt.stateStore == nil {
		return emitErr
	}
	return errors.Join(emitErr, rt.stateStore.SaveExecution(ctx, state.Execution{
		ExecID:      session.ExecID,
		TraceID:     session.TraceID,
		Status:      stateStatus,
		StartedAt:   session.StartedAt,
		CompletedAt: time.Now().UTC(),
	}))
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
		specs, closeMCP, err := rt.registerStepTools(ctx, executor, step)
		if err != nil {
			return result, err
		}
		stepProvider, err := rt.providerForModel(step.Model)
		if err != nil {
			_ = closeMCP()
			return result, err
		}
		harnessOptions := []harness.Option{
			harness.WithModel(step.Model),
			harness.WithSystemPrompt(step.Goal),
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
		if scope.parentSession != nil {
			harnessOptions = append(harnessOptions, harness.WithParentSession(*scope.parentSession))
		}
		if scope.budgetLedger != nil {
			harnessOptions = append(harnessOptions, harness.WithBudgetLedger(scope.budgetLedger))
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
