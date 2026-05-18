package ovr

import (
	"context"
	"errors"
	"fmt"

	"ouvrier/internal/harness"
	"ouvrier/internal/provider"
	runtimeplan "ouvrier/internal/runtime"
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
}

func defaultHTTPRuntime() httpRuntime {
	providers, _ := providerRegistryFromEnv()
	return httpRuntime{
		providers:    providers,
		toolExecutor: tools.NewExecutor(),
	}
}

func (rt httpRuntime) runPlan(ctx context.Context, plan runtimeplan.Plan, input string) (string, error) {
	if len(plan.Steps) == 0 {
		return input, nil
	}
	executor := rt.toolExecutor
	if executor == nil {
		executor = tools.NewExecutor()
	}

	current := input
	for _, step := range plan.Steps {
		specs, err := registerRuntimeTools(executor, step.Tools)
		if err != nil {
			return "", err
		}
		stepProvider, err := rt.providerForModel(step.Model)
		if err != nil {
			return "", err
		}
		h, err := harness.New(stepProvider,
			harness.WithModel(step.Model),
			harness.WithToolExecutor(executor),
			harness.WithTools(specs...),
		)
		if err != nil {
			return "", err
		}
		out, err := h.Run(ctx, current)
		if err != nil {
			return "", err
		}
		if out.Status != harness.StatusCompleted {
			return "", fmt.Errorf("%w: %s", errHTTPPipelineIncomplete, out.Status)
		}
		current = out.Text
	}
	return current, nil
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
