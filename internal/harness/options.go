package harness

import (
	"errors"
	"strings"

	"ouvrier/internal/tools"
)

const DefaultMaxIterations = 25

type Option func(*config) error

type config struct {
	model         string
	systemPrompt  string
	maxIterations int
	toolExecutor  *tools.Executor
}

func defaultConfig() config {
	return config{
		maxIterations: DefaultMaxIterations,
		toolExecutor:  tools.NewExecutor(),
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
		cfg.maxIterations = max
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
