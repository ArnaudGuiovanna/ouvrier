package provider_test

import (
	"context"
	"errors"
	"testing"

	"ouvrier/internal/provider"
)

type registryProvider struct {
	name string
}

func (p registryProvider) Name() string {
	return p.name
}

func (p registryProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	return provider.Response{Text: p.name, StopReason: provider.StopEndTurn}, nil
}

func TestRegistryRoutesByModelProviderPrefix(t *testing.T) {
	registry, err := provider.NewRegistry(
		registryProvider{name: "anthropic"},
		registryProvider{name: "openai"},
	)
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}

	p, err := registry.ForModel("openai/gpt-4.1-mini")
	if err != nil {
		t.Fatalf("ForModel returned error: %v", err)
	}
	if p.Name() != "openai" {
		t.Fatalf("provider = %q, want openai", p.Name())
	}
}

func TestRegistryRoutesSlashyModelNames(t *testing.T) {
	registry, err := provider.NewRegistry(registryProvider{name: "vllm"})
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}

	p, err := registry.ForModel("vllm/meta-llama/Llama-3.1-8B-Instruct")
	if err != nil {
		t.Fatalf("ForModel returned error: %v", err)
	}
	if p.Name() != "vllm" {
		t.Fatalf("provider = %q, want vllm", p.Name())
	}
}

func TestRegistryRejectsMissingProvider(t *testing.T) {
	registry, err := provider.NewRegistry(registryProvider{name: "anthropic"})
	if err != nil {
		t.Fatalf("NewRegistry returned error: %v", err)
	}

	_, err = registry.ForModel("openai/gpt-4.1-mini")
	if !errors.Is(err, provider.ErrProviderNotConfigured) {
		t.Fatalf("ForModel error = %v, want ErrProviderNotConfigured", err)
	}
}

func TestRegistryRejectsInvalidProviders(t *testing.T) {
	tests := []struct {
		name string
		p    provider.Provider
	}{
		{name: "nil", p: nil},
		{name: "empty name", p: registryProvider{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := provider.NewRegistry(tt.p)
			if err == nil {
				t.Fatal("NewRegistry returned nil error")
			}
		})
	}
}
