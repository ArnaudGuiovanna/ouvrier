package harness

import (
	"errors"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type providerMaxTokensError struct {
	outputTokens int
}

func (e *providerMaxTokensError) Error() string {
	return "provider stopped at max tokens"
}

func providerMaxTokensErrorPayload(err error) (map[string]any, bool) {
	var stopped *providerMaxTokensError
	if !errors.As(err, &stopped) || stopped == nil {
		return nil, false
	}
	return providerMaxTokensBudgetPayload(stopped.outputTokens), true
}

func providerMaxTokensBudgetPayload(outputTokens int) map[string]any {
	return map[string]any{
		"budget":        "provider_max_tokens",
		"stop_reason":   string(provider.StopMaxTokens),
		"output_tokens": outputTokens,
	}
}
