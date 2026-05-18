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
	parts := strings.Split(raw, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return ModelRef{}, fmt.Errorf("model ID %q must use provider/model form", raw)
	}
	return ModelRef{
		Provider: strings.TrimSpace(parts[0]),
		Name:     strings.TrimSpace(parts[1]),
	}, nil
}
