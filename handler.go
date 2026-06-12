package ovr

import (
	"errors"
	"net/http"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

// Provider is the LLM boundary the runtime calls for completions. Implement it
// — or use the ovrtest package's scripted provider — to drive a worker in tests
// without contacting a real model and without spending tokens. In production,
// providers are configured from environment credentials and this need not be
// set.
type Provider = provider.Provider

// WithProvider injects a single Provider used for every model id, bypassing the
// environment-derived provider registry. It is the seam that makes a worker
// unit-testable: pair it with Handler and httptest, or with a scripted provider
// from the ovrtest package.
func WithProvider(p Provider) RunnerOption {
	return func(cfg *runnerConfig) {
		if p == nil {
			cfg.setErr(errors.New("provider is required"))
			return
		}
		cfg.provider = p
	}
}

// Handler compiles nodes into an http.Handler for HTTP and Webhook triggers,
// ready to drive with net/http/httptest. It mirrors what Run serves, minus the
// listener, so a worker can be exercised end to end in a Go test. Cron and
// stream triggers are not served by the returned handler.
//
// The handler uses an in-memory state store unless the Runner was built
// WithStateStore. Configure a Provider with WithProvider to avoid live model
// calls.
func Handler(nodes ...Node) (http.Handler, error) {
	return NewRunner().Handler(nodes...)
}

// Handler builds an http.Handler from this Runner's configuration. See the
// package-level Handler for details.
func (r *Runner) Handler(nodes ...Node) (http.Handler, error) {
	if r == nil {
		return nil, errors.New("runner is required")
	}
	if r.err != nil {
		return nil, r.err
	}
	if err := checkLegacyEnv(); err != nil {
		return nil, err
	}
	if _, err := compilePlans(nodes); err != nil {
		return nil, err
	}
	rt, err := r.handlerRuntime()
	if err != nil {
		return nil, err
	}
	return newHTTPCompatibleHandlerWithRuntime(nodes, rt)
}

// handlerRuntime assembles an httpRuntime for Handler, defaulting to an
// in-memory state store so tests neither touch disk nor require env setup.
func (r *Runner) handlerRuntime() (httpRuntime, error) {
	rt := defaultHTTPRuntime()
	if r.stateStore != nil {
		rt.stateStore = publicStateStoreAdapter{store: r.stateStore}
	} else {
		rt.stateStore = state.NewMemoryStore()
	}
	if err := seedHTTPEventStreamFromStore(&rt, rt.stateStore); err != nil {
		return httpRuntime{}, err
	}
	if err := r.configureHTTPRuntime(&rt); err != nil {
		return httpRuntime{}, err
	}
	return rt, nil
}
