package provider

import (
	"fmt"
	"strings"
)

type ModelRef struct {
	Provider string
	Name     string
}

func ParseModelID(raw string) (ModelRef, error) {
	provider, name, ok := strings.Cut(raw, "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(name) == "" {
		return ModelRef{}, fmt.Errorf("model ID %q must use provider/model form", raw)
	}
	return ModelRef{
		Provider: strings.TrimSpace(provider),
		Name:     strings.TrimSpace(name),
	}, nil
}
