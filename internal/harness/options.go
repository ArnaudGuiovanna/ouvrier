package harness

import (
	"errors"
	"strings"
)

const DefaultMaxIterations = 25

type Option func(*config) error

type config struct {
	model         string
	systemPrompt  string
	maxIterations int
}

func defaultConfig() config {
	return config{maxIterations: DefaultMaxIterations}
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
