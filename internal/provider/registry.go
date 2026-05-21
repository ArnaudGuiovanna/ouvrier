package provider

import (
	"errors"
	"fmt"
	"strings"
)

var ErrProviderNotConfigured = errors.New("provider not configured")

type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	registry := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) Register(provider Provider) error {
	if provider == nil {
		return errors.New("provider is required")
	}
	name := strings.TrimSpace(provider.Name())
	if name == "" {
		return errors.New("provider name is required")
	}
	if r.providers == nil {
		r.providers = make(map[string]Provider)
	}
	r.providers[name] = provider
	return nil
}

func (r *Registry) ForModel(model string) (Provider, error) {
	ref, err := ParseModelID(model)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotConfigured, ref.Provider)
	}
	provider := r.providers[ref.Provider]
	if provider == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderNotConfigured, ref.Provider)
	}
	return provider, nil
}
