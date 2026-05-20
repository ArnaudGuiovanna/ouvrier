package ovr

import (
	"errors"
	"fmt"

	"ouvrier/internal/tools"
)

// Runner owns advanced runtime configuration for Ouvrier pipelines.
type Runner struct {
	permissionPolicy PermissionPolicy
	err              error
}

type runnerConfig struct {
	permissionPolicy PermissionPolicy
	err              error
}

// RunnerOption configures a Runner.
type RunnerOption func(*runnerConfig)

// NewRunner creates an advanced runner. Configuration errors are returned by Run.
func NewRunner(options ...RunnerOption) *Runner {
	cfg := runnerConfig{}
	for _, option := range options {
		if option == nil {
			cfg.setErr(errors.New("nil runner option"))
			continue
		}
		option(&cfg)
	}
	return &Runner{
		permissionPolicy: cfg.permissionPolicy,
		err:              cfg.err,
	}
}

// WithPermissionPolicy installs a production permission policy for privileged actions.
func WithPermissionPolicy(permissionPolicy PermissionPolicy) RunnerOption {
	return func(cfg *runnerConfig) {
		if permissionPolicy == nil {
			cfg.setErr(errors.New("permission policy is required"))
			return
		}
		cfg.permissionPolicy = permissionPolicy
	}
}

// Run validates a pipeline declaration before starting the runtime.
func (r *Runner) Run(addr string, nodes ...Node) error {
	if r == nil {
		return errors.New("runner is required")
	}
	if r.err != nil {
		return r.err
	}
	if err := validatePipeline(nodes); err != nil {
		return err
	}

	runtime, closeRuntime, err := r.defaultHTTPRuntimeForRun()
	if err != nil {
		return fmt.Errorf("state store: %w", err)
	}

	handler, err := newHTTPHandlerWithRuntime(nodes, runtime)
	if err != nil {
		_ = closeRuntime()
		return err
	}
	serveErr := serveHTTP(addr, handler)
	closeErr := closeRuntime()
	return errors.Join(serveErr, closeErr)
}

func (r *Runner) defaultHTTPRuntimeForRun() (httpRuntime, func() error, error) {
	rt, closeRuntime, err := defaultHTTPRuntimeForRun()
	if err != nil {
		return httpRuntime{}, nil, err
	}
	if err := r.configureHTTPRuntime(&rt); err != nil {
		_ = closeRuntime()
		return httpRuntime{}, nil, err
	}
	return rt, closeRuntime, nil
}

func (r *Runner) configureHTTPRuntime(rt *httpRuntime) error {
	if r == nil {
		return errors.New("runner is required")
	}
	if r.err != nil {
		return r.err
	}
	if r.permissionPolicy != nil {
		rt.toolExecutor = tools.NewExecutor(tools.WithPermissionPolicy(
			internalPermissionPolicyAdapter{public: r.permissionPolicy},
		))
	}
	return nil
}

func (cfg *runnerConfig) setErr(err error) {
	if cfg.err == nil {
		cfg.err = err
	}
}
