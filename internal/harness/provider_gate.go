package harness

import (
	"context"
	"strings"
	"sync"
)

// ProviderGate bounds the number of concurrent in-flight LLM calls per
// provider. A single gate is shared across every harness that runs against the
// same provider registry, so one provider hitting its rate limit cannot starve
// calls routed to a different provider. A zero or negative limit for a provider
// means unbounded.
type ProviderGate struct {
	mu        sync.Mutex
	limits    map[string]int
	semaphore map[string]chan struct{}
}

// NewProviderGate builds a gate from a per-provider maximum in-flight map. Keys
// are provider names (the part before "/" in a model id). Limits less than or
// equal to zero leave that provider unbounded.
func NewProviderGate(limits map[string]int) *ProviderGate {
	copied := make(map[string]int, len(limits))
	for name, limit := range limits {
		name = strings.TrimSpace(name)
		if name == "" || limit <= 0 {
			continue
		}
		copied[name] = limit
	}
	if len(copied) == 0 {
		return nil
	}
	return &ProviderGate{
		limits:    copied,
		semaphore: make(map[string]chan struct{}, len(copied)),
	}
}

func (g *ProviderGate) channel(providerName string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	limit, ok := g.limits[providerName]
	if !ok || limit <= 0 {
		return nil
	}
	sem := g.semaphore[providerName]
	if sem == nil {
		sem = make(chan struct{}, limit)
		g.semaphore[providerName] = sem
	}
	return sem
}

// Acquire blocks until a slot for providerName is free or ctx is done. It
// returns a release func that must be called when the in-flight call finishes.
// Providers without a configured limit acquire instantly with a no-op release.
func (g *ProviderGate) Acquire(ctx context.Context, providerName string) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	providerName = strings.TrimSpace(providerName)
	sem := g.channel(providerName)
	if sem == nil {
		return func() {}, nil
	}
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}
