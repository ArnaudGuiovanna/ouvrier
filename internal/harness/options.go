package harness

import (
	"errors"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

const (
	DefaultMaxIterations   = 25
	DefaultMaxTokens       = 500_000
	DefaultMaxCostUSD      = 5.00
	DefaultMaxWallClock    = 10 * time.Minute
	DefaultProviderRetries = 3
)

type Option func(*config) error

type config struct {
	model           string
	systemPrompt    string
	budget          runtimecore.Budget
	budgetSet       bool
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
	pricing         provider.PricingTable
	streamDeltas    bool
}

func defaultConfig() config {
	return config{
		budget: runtimecore.Budget{
			MaxIterations: DefaultMaxIterations,
			MaxTokens:     DefaultMaxTokens,
			MaxCostUSD:    DefaultMaxCostUSD,
			MaxWallClock:  DefaultMaxWallClock,
		},
		toolExecutor:    tools.NewExecutor(),
		providerRetries: DefaultProviderRetries,
		promptCache:     true,
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
		cfg.budgetSet = true
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
		if budget.MaxWallClock < 0 {
			return errors.New("max wallclock must be greater than or equal to zero")
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
		if budget.MaxWallClock == 0 {
			budget.MaxWallClock = cfg.budget.MaxWallClock
		}
		cfg.budget = budget
		cfg.budgetSet = true
		return nil
	}
}

func WithParentSession(parent runtimecore.Session) Option {
	return func(cfg *config) error {
		if parent.ExecID == "" || parent.SessionID == "" || parent.TraceID == "" {
			return errors.New("parent session must include exec, session, and trace IDs")
		}
		parentCopy := parent
		cfg.parentSession = &parentCopy
		if !cfg.budgetSet {
			cfg.budget = parent.Budget
		}
		return nil
	}
}

func WithBudgetLedger(ledger *BudgetLedger) Option {
	return func(cfg *config) error {
		if ledger == nil {
			return errors.New("budget ledger is required")
		}
		cfg.budgetLedger = ledger
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

func WithPromptCache(enabled bool) Option {
	return func(cfg *config) error {
		cfg.promptCache = enabled
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

func WithSchemaRepairAttempts(max int) Option {
	return func(cfg *config) error {
		if max < 0 {
			return errors.New("schema repair attempts must be greater than or equal to zero")
		}
		cfg.schemaRepairs = max
		return nil
	}
}

// WithStreaming enables provider token-delta streaming. When enabled and the
// configured provider implements provider.StreamingProvider, the harness emits
// an EventLLMTokenDelta event per token chunk while a completion is in flight.
// Providers without streaming support fall back to Complete transparently.
func WithStreaming(enabled bool) Option {
	return func(cfg *config) error {
		cfg.streamDeltas = enabled
		return nil
	}
}

func WithSequentialTools() Option {
	return func(cfg *config) error {
		cfg.sequentialTools = true
		return nil
	}
}

// WithPricing installs a pricing table used to compute Usage.CostUSD per call.
// When unset (nil/empty), cost stays best-effort and is left untouched.
func WithPricing(table provider.PricingTable) Option {
	return func(cfg *config) error {
		cfg.pricing = table
		return nil
	}
}
