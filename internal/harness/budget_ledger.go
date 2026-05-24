package harness

import (
	"sync"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

type BudgetLedger struct {
	mu     sync.Mutex
	budget runtimecore.Budget
	usage  provider.Usage
}

func NewBudgetLedger(budget runtimecore.Budget) *BudgetLedger {
	return &BudgetLedger{budget: budget}
}

func (l *BudgetLedger) Add(usage provider.Usage) (provider.Usage, map[string]any, bool) {
	if l == nil {
		return usage, nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.usage.Add(usage)
	payload, exceeded := budgetPayload(l.budget, l.usage)
	return l.usage, payload, exceeded
}

func (l *BudgetLedger) Exceeded() (provider.Usage, map[string]any, bool) {
	if l == nil {
		return provider.Usage{}, nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	payload, exceeded := budgetPayload(l.budget, l.usage)
	return l.usage, payload, exceeded
}

func (l *BudgetLedger) Snapshot() provider.Usage {
	if l == nil {
		return provider.Usage{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usage
}

func budgetPayload(budget runtimecore.Budget, usage provider.Usage) (map[string]any, bool) {
	usedTokens := usage.InputTokens + usage.OutputTokens
	if budget.MaxTokens > 0 && usedTokens > budget.MaxTokens {
		return map[string]any{
			"budget":        "tokens",
			"max_tokens":    budget.MaxTokens,
			"used_tokens":   usedTokens,
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"max_cost_usd":  budget.MaxCostUSD,
			"used_cost_usd": usage.CostUSD,
		}, true
	}
	if budget.MaxCostUSD > 0 && usage.CostUSD > budget.MaxCostUSD {
		return map[string]any{
			"budget":        "cost_usd",
			"max_tokens":    budget.MaxTokens,
			"used_tokens":   usedTokens,
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
			"max_cost_usd":  budget.MaxCostUSD,
			"used_cost_usd": usage.CostUSD,
		}, true
	}
	return nil, false
}
