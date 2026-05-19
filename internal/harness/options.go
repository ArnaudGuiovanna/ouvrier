package harness

import (
	"errors"
	"strings"
	"time"

	"ouvrier/internal/events"
	"ouvrier/internal/provider"
	runtimecore "ouvrier/internal/runtime"
	"ouvrier/internal/state"
	"ouvrier/internal/tools"
)

const (
	DefaultMaxIterations   = 25
	DefaultMaxTokens       = 500_000
	DefaultMaxCostUSD      = 5.00
	DefaultProviderRetries = 3
)

type Option func(*config) error

type config struct {
	model           string
	systemPrompt    string
	budget          runtimecore.Budget
	toolExecutor    *tools.Executor
	tools           []provider.ToolSpec
	stateStore      state.Store
	eventStream     *events.EventStream
	hookBus         *events.HookBus
	resultSchema    *runtimecore.ResultSchema
	providerRetries int
	retryBackoff    time.Duration
}

func defaultConfig() config {
	return config{
		budget: runtimecore.Budget{
			MaxIterations: DefaultMaxIterations,
			MaxTokens:     DefaultMaxTokens,
			MaxCostUSD:    DefaultMaxCostUSD,
		},
		toolExecutor:    tools.NewExecutor(),
		providerRetries: DefaultProviderRetries,
	}
}

func WithModel(model string) Option {
	return func(cfg *config) error {
		cfg.model = strings.TrimSpace(model)
		return nil
	}
}

func WithSystemPrompt(prompt string) Option {
	return func(cfg *config) error {
		cfg.systemPrompt = prompt
		return nil
	}
}

func WithMaxIterations(max int) Option {
	return func(cfg *config) error {
		if max <= 0 {
			return errors.New("max iterations must be greater than zero")
		}
		cfg.budget.MaxIterations = max
		return nil
	}
}

func WithBudget(budget runtimecore.Budget) Option {
	return func(cfg *config) error {
		if budget.MaxIterations < 0 {
			return errors.New("max iterations must be greater than zero")
		}
		if budget.MaxTokens < 0 {
			return errors.New("max tokens must be greater than or equal to zero")
		}
		if budget.MaxCostUSD < 0 {
			return errors.New("max cost USD must be greater than or equal to zero")
		}
		if budget.MaxIterations == 0 {
			budget.MaxIterations = cfg.budget.MaxIterations
		}
		if budget.MaxTokens == 0 {
			budget.MaxTokens = cfg.budget.MaxTokens
		}
		if budget.MaxCostUSD == 0 {
			budget.MaxCostUSD = cfg.budget.MaxCostUSD
		}
		cfg.budget = budget
		return nil
	}
}

func WithProviderRetries(max int) Option {
	return func(cfg *config) error {
		if max < 0 {
			return errors.New("provider retries must be greater than or equal to zero")
		}
		cfg.providerRetries = max
		return nil
	}
}

func WithRetryBackoff(backoff time.Duration) Option {
	return func(cfg *config) error {
		if backoff < 0 {
			return errors.New("retry backoff must be greater than or equal to zero")
		}
		cfg.retryBackoff = backoff
		return nil
	}
}

func WithToolExecutor(executor *tools.Executor) Option {
	return func(cfg *config) error {
		if executor == nil {
			return errors.New("tool executor is required")
		}
		cfg.toolExecutor = executor
		return nil
	}
}

func WithTools(specs ...provider.ToolSpec) Option {
	return func(cfg *config) error {
		cfg.tools = append([]provider.ToolSpec(nil), specs...)
		return nil
	}
}

func WithStateStore(store state.Store) Option {
	return func(cfg *config) error {
		if store == nil {
			return errors.New("state store is required")
		}
		cfg.stateStore = store
		return nil
	}
}

func WithEventStream(stream *events.EventStream) Option {
	return func(cfg *config) error {
		if stream == nil {
			return errors.New("event stream is required")
		}
		cfg.eventStream = stream
		return nil
	}
}

func WithHookBus(bus *events.HookBus) Option {
	return func(cfg *config) error {
		if bus == nil {
			return errors.New("hook bus is required")
		}
		cfg.hookBus = bus
		return nil
	}
}

func WithResultSchema(contract *runtimecore.ResultSchema) Option {
	return func(cfg *config) error {
		if contract == nil {
			return errors.New("result schema is required")
		}
		cfg.resultSchema = contract
		return nil
	}
}
