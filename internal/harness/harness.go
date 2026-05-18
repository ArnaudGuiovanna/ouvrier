package harness

import (
	"context"
	"errors"

	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/tools"
)

type Harness struct {
	provider      provider.Provider
	model         string
	systemPrompt  string
	maxIterations int
	toolExecutor  *tools.Executor
	tools         []provider.ToolSpec
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
	return &Harness{
		provider:      p,
		model:         cfg.model,
		systemPrompt:  cfg.systemPrompt,
		maxIterations: cfg.maxIterations,
		toolExecutor:  cfg.toolExecutor,
		tools:         append([]provider.ToolSpec(nil), cfg.tools...),
	}, nil
}

func (h *Harness) Run(ctx context.Context, input string) (Outcome, error) {
	session, err := runtimecore.NewSession(h.model,
		runtimecore.WithSessionBudget(runtimecore.Budget{MaxIterations: h.maxIterations}),
	)
	if err != nil {
		return Outcome{Status: StatusFailed}, err
	}

	messages := []provider.Message{provider.UserText(input)}
	out := Outcome{Session: session}

	for out.Iterations < h.maxIterations {
		out.Iterations++
		resp, err := h.provider.Complete(ctx, provider.Request{
			Model:    h.model,
			System:   h.systemPrompt,
			Messages: append([]provider.Message(nil), messages...),
			Tools:    append([]provider.ToolSpec(nil), h.tools...),
		})
		if err != nil {
			out.Status = StatusFailed
			return out, err
		}

		out.Usage.Add(resp.Usage)
		if resp.Text != "" {
			out.Text = resp.Text
		}
		if len(resp.ToolCalls) == 0 {
			out.Status = StatusCompleted
			return out, nil
		}

		out.ToolCalls = append(out.ToolCalls, resp.ToolCalls...)
		messages = append(messages, provider.AssistantToolCalls(resp.Text, resp.ToolCalls...))
		for _, call := range resp.ToolCalls {
			result, err := h.toolExecutor.Execute(ctx, call)
			if err != nil {
				messages = append(messages, provider.ToolResultText(call, err.Error(), true))
				continue
			}
			messages = append(messages, provider.ToolResultMessage(result))
		}
	}

	out.Status = StatusTruncated
	return out, nil
}
