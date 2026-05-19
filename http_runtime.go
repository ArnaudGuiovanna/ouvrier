package ovr

import (
	"context"
	"errors"
	"fmt"

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
	provider     provider.Provider
	providers    *provider.Registry
	toolExecutor *tools.Executor
	mcpConnector mcpConnector
	stateStore   state.Store
	eventStream  *events.EventStream
	hookBus      *events.HookBus
	adminToken   string
	adminRoutes  []httpRoute
}

func defaultHTTPRuntime() httpRuntime {
	providers, _ := providerRegistryFromEnv()
	stream, _ := events.NewEventStream()
	return httpRuntime{
		providers:    providers,
		toolExecutor: tools.NewExecutor(),
		mcpConnector: envMCPConnector{connector: mcpclient.NewEnvConnector()},
		eventStream:  stream,
	}
}

func defaultHTTPRuntimeForRun() (httpRuntime, func() error, error) {
	rt := defaultHTTPRuntime()
	store, err := state.NewStoreFromEnv()
	if err != nil {
		return httpRuntime{}, nil, err
	}
	rt.stateStore = store
	return rt, func() error {
		closer, ok := store.(interface{ Close() error })
		if !ok {
			return nil
		}
		return closer.Close()
	}, nil
}

func (rt httpRuntime) runPlan(ctx context.Context, plan runtimeplan.Plan, input string) (string, error) {
	result, err := rt.runPlanResult(ctx, plan, input)
	return result.Output, err
}

func (rt httpRuntime) runPlanResult(ctx context.Context, plan runtimeplan.Plan, input string) (planRunResult, error) {
	return rt.runStepsResult(ctx, plan.Steps, input, planRunScope{})
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
