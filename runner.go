package ovr

import (
	"errors"
	"fmt"
	"os"
	"strings"

	runtimeplan "ouvrier/internal/runtime"
	internalsandbox "ouvrier/internal/sandbox"
	"ouvrier/internal/tools"
)

// Runner owns advanced runtime configuration for Ouvrier pipelines.
type Runner struct {
	permissionPolicy PermissionPolicy
	stateStore       StateStore
	hooks            *Hooks
	sandbox          SandboxConfig
	err              error
}

type runnerConfig struct {
	permissionPolicy PermissionPolicy
	stateStore       StateStore
	hooks            *Hooks
	sandbox          SandboxConfig
	err              error
}

// SandboxConfig describes an explicit filesystem workspace boundary.
type SandboxConfig struct {
	root       string
	allowedEnv []string
}

// Sandbox configures a workspace root used for filesystem-bound capabilities.
func Sandbox(root string, options ...SandboxOption) SandboxConfig {
	config := SandboxConfig{root: strings.TrimSpace(root)}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return config
}

// SandboxOption configures a sandbox boundary.
type SandboxOption func(*SandboxConfig)

// AllowEnv allows selected environment variables inside sandboxed capabilities.
func AllowEnv(keys ...string) SandboxOption {
	return func(config *SandboxConfig) {
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key != "" {
				config.allowedEnv = append(config.allowedEnv, key)
			}
		}
	}
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
		stateStore:       cfg.stateStore,
		hooks:            cfg.hooks,
		sandbox:          cfg.sandbox,
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

// WithStateStore installs a custom durable execution store.
func WithStateStore(store StateStore) RunnerOption {
	return func(cfg *runnerConfig) {
		if store == nil {
			cfg.setErr(errors.New("state store is required"))
			return
		}
		cfg.stateStore = store
	}
}

// WithHooks installs advanced lifecycle hooks for the runner.
func WithHooks(hooks *Hooks) RunnerOption {
	return func(cfg *runnerConfig) {
		if hooks == nil {
			cfg.setErr(errors.New("hooks are required"))
			return
		}
		cfg.hooks = hooks
	}
}

// WithSandbox installs a filesystem workspace boundary for the runner.
func WithSandbox(sandbox SandboxConfig) RunnerOption {
	return func(cfg *runnerConfig) {
		if sandbox.root == "" {
			cfg.setErr(errors.New("sandbox root is required"))
			return
		}
		cfg.sandbox = sandbox
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
	plans, err := compilePlans(nodes)
	if err != nil {
		return err
	}

	runtime, closeRuntime, err := r.defaultHTTPRuntimeForRun()
	if err != nil {
		return fmt.Errorf("state store: %w", err)
	}

	var serveErr error
	if plansHTTPCompatible(plans) {
		handler, err := newHTTPCompatibleHandlerWithRuntime(nodes, runtime)
		if err != nil {
			_ = closeRuntime()
			return err
		}
		serveErr = serveHTTP(addr, handler)
	} else if plansTriggerKind(plans) == runtimeplan.TriggerCron {
		serveErr = serveCronPlans(addr, runtime, plans)
	} else if plansTriggerKind(plans) == runtimeplan.TriggerStream {
		serveErr = serveStreamPlans(addr, runtime, plans)
	} else if plansRunnableTogether(plans) {
		serveErr = serveMixedPlans(addr, runtime, plans)
	} else {
		serveErr = fmt.Errorf("%w: mixed or unsupported trigger runtime", ErrRunNotImplemented)
	}
	closeErr := closeRuntime()
	return errors.Join(serveErr, closeErr)
}

func plansHTTPCompatible(plans []runtimeplan.Plan) bool {
	if len(plans) == 0 {
		return false
	}
	for _, plan := range plans {
		switch plan.Trigger.Kind {
		case runtimeplan.TriggerHTTP, runtimeplan.TriggerWebhook:
		default:
			return false
		}
	}
	return true
}

func plansTriggerKind(plans []runtimeplan.Plan) runtimeplan.TriggerKind {
	if len(plans) == 0 {
		return ""
	}
	kind := plans[0].Trigger.Kind
	for _, plan := range plans[1:] {
		if plan.Trigger.Kind != kind {
			return ""
		}
	}
	return kind
}

func (r *Runner) defaultHTTPRuntimeForRun() (httpRuntime, func() error, error) {
	var (
		rt           httpRuntime
		closeRuntime func() error
		err          error
	)
	if r != nil && r.stateStore != nil {
		rt = defaultHTTPRuntime()
		rt.stateStore = publicStateStoreAdapter{store: r.stateStore}
		closeRuntime = func() error {
			closer, ok := r.stateStore.(interface{ Close() error })
			if !ok {
				return nil
			}
			return closer.Close()
		}
	} else {
		rt, closeRuntime, err = defaultHTTPRuntimeForRun()
		if err != nil {
			return httpRuntime{}, nil, err
		}
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
	if r.hooks != nil {
		hookBus, err := r.hooks.hookBus()
		if err != nil {
			return err
		}
		rt.hookBus = hookBus
	}
	if r.sandbox.root != "" {
		sandbox, err := internalsandbox.New(r.sandbox.root,
			internalsandbox.WithEnvironment(currentEnvironment()),
			internalsandbox.WithAllowedEnv(r.sandbox.allowedEnv...),
		)
		if err != nil {
			return err
		}
		rt.sandbox = sandbox
	}
	return nil
}

func currentEnvironment() map[string]string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}

func (cfg *runnerConfig) setErr(err error) {
	if cfg.err == nil {
		cfg.err = err
	}
}
