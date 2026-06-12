package ovr

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	internalsandbox "github.com/ArnaudGuiovanna/ouvrier/internal/sandbox"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// Tracer is the public surface for OTel-compatible span emission.
// Implementations are free to use OpenTelemetry, Jaeger, Honeycomb, or any
// other observability stack. The harness emits one span per pipeline, pipe,
// session, LLM call, tool call, schema validation, and subagent task.
type Tracer = events.Tracer

// Span is a single tracing span minted by a Tracer.
type Span = events.Span

// NopTracer returns a Tracer whose spans do nothing. Useful for tests or for
// disabling tracing without removing the WithTracer option.
func NopTracer() Tracer { return events.NopTracer() }

// Runner owns advanced runtime configuration for Ouvrier pipelines.
type Runner struct {
	permissionPolicy     PermissionPolicy
	stateStore           StateStore
	hooks                *Hooks
	sandbox              SandboxConfig
	schemaRepairAttempts int
	tracer               Tracer
	pricing              PricingTable
	providerBudgets      map[string]int
	provider             Provider
	err                  error
}

type runnerConfig struct {
	permissionPolicy     PermissionPolicy
	stateStore           StateStore
	hooks                *Hooks
	sandbox              SandboxConfig
	schemaRepairAttempts int
	tracer               Tracer
	pricing              PricingTable
	providerBudgets      map[string]int
	provider             Provider
	err                  error
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
		permissionPolicy:     cfg.permissionPolicy,
		stateStore:           cfg.stateStore,
		hooks:                cfg.hooks,
		sandbox:              cfg.sandbox,
		schemaRepairAttempts: cfg.schemaRepairAttempts,
		tracer:               cfg.tracer,
		pricing:              cfg.pricing,
		providerBudgets:      cfg.providerBudgets,
		provider:             cfg.provider,
		err:                  cfg.err,
	}
}

// WithTracer installs a Tracer that receives span lifecycle events for
// pipeline, pipe, session, LLM, tool, schema, and subagent activity. The
// harness pairs *_started events with their *_completed or *_failed
// counterparts internally; callers see one span per logical operation.
func WithTracer(tracer Tracer) RunnerOption {
	return func(cfg *runnerConfig) {
		if tracer == nil {
			cfg.setErr(errors.New("tracer is required"))
			return
		}
		cfg.tracer = tracer
	}
}

// WithOTLPExporter installs a Tracer that ships spans to an OTLP/HTTP
// collector at endpoint (e.g. "https://collector:4318"); the exporter appends
// "/v1/traces". Spans are encoded as OTLP/HTTP JSON with no heavy otel SDK
// dependency, and attributes are redacted before export. It is a convenience
// wrapper over WithTracer; passing both means the last option wins. An empty
// endpoint is rejected. Default off: when this option is unset, behavior is
// unchanged.
func WithOTLPExporter(endpoint string, options ...OTLPExporterOption) RunnerOption {
	return func(cfg *runnerConfig) {
		opts := make([]events.OTLPOption, 0, len(options))
		for _, option := range options {
			if option != nil {
				opts = append(opts, events.OTLPOption(option))
			}
		}
		exporter, err := events.NewOTLPExporter(endpoint, opts...)
		if err != nil {
			cfg.setErr(err)
			return
		}
		cfg.tracer = exporter
	}
}

// OTLPExporterOption configures the OTLP exporter installed by WithOTLPExporter.
type OTLPExporterOption = events.OTLPOption

// OTLPServiceName sets the service.name resource attribute on exported spans.
func OTLPServiceName(name string) OTLPExporterOption {
	return events.WithOTLPServiceName(name)
}

// OTLPHeaders attaches additional HTTP headers (e.g. authorization for hosted
// collectors) to every OTLP export request.
func OTLPHeaders(headers map[string]string) OTLPExporterOption {
	return events.WithOTLPHeaders(headers)
}

// WithPricing installs a pricing table used to compute per-call and
// per-execution cost. When unset, cost stays best-effort (zero) with no
// behavior change.
func WithPricing(table PricingTable) RunnerOption {
	return func(cfg *runnerConfig) {
		if len(table) == 0 {
			cfg.setErr(errors.New("pricing table is required"))
			return
		}
		cfg.pricing = table
	}
}

// WithProviderBudget bounds the number of concurrent in-flight LLM calls for a
// single provider (the part before "/" in a model id). It prevents one
// provider's rate limit from stalling calls routed to other providers. The
// budget is shared across every Pipe in the runner. A limit less than or equal
// to zero is rejected.
func WithProviderBudget(provider string, maxInFlight int) RunnerOption {
	return func(cfg *runnerConfig) {
		provider = strings.TrimSpace(provider)
		if provider == "" {
			cfg.setErr(errors.New("provider budget name is required"))
			return
		}
		if maxInFlight <= 0 {
			cfg.setErr(errors.New("provider budget max in-flight must be greater than zero"))
			return
		}
		if cfg.providerBudgets == nil {
			cfg.providerBudgets = make(map[string]int)
		}
		cfg.providerBudgets[provider] = maxInFlight
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

// WithSchemaRepairAttempts enables bounded ResultSchema repair attempts for runtime Pipe outputs.
func WithSchemaRepairAttempts(max int) RunnerOption {
	return func(cfg *runnerConfig) {
		if max < 0 {
			cfg.setErr(errors.New("schema repair attempts must be greater than or equal to zero"))
			return
		}
		cfg.schemaRepairAttempts = max
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
	if err := checkLegacyEnv(); err != nil {
		return err
	}
	plans, err := compilePlans(nodes)
	if err != nil {
		return err
	}

	runtime, closeRuntime, err := r.defaultHTTPRuntimeForRun()
	if err != nil {
		return fmt.Errorf("state store: %w", err)
	}

	if err := checkAdminExposure(addr, runtime.adminToken); err != nil {
		_ = closeRuntime()
		return err
	}
	if warning := adminExposureWarning(addr, runtime.adminToken); warning != "" {
		log.Println(warning)
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
		// Startup guard: custom public stores cannot persist the run
		// journal, so the durable-runs flag is refused loudly here.
		durable, err := durableRunsConfigForStore(rt.stateStore)
		if err != nil {
			_ = closeRuntime()
			return httpRuntime{}, nil, err
		}
		rt.durableRuns = durable
		if err := seedHTTPEventStreamFromStore(&rt, rt.stateStore); err != nil {
			_ = closeRuntime()
			return httpRuntime{}, nil, err
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
	if r.tracer != nil && rt.eventStream != nil {
		if err := rt.eventStream.Subscribe(events.TracerSubscriber(r.tracer)); err != nil {
			return err
		}
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
	rt.schemaRepairAttempts = r.schemaRepairAttempts
	if len(r.pricing) > 0 {
		rt.pricing = r.pricing
	}
	if gate := harness.NewProviderGate(r.providerBudgets); gate != nil {
		rt.providerGate = gate
	}
	if r.provider != nil {
		// An injected provider serves every model id; clear the env-derived
		// registry so resolution falls through to it (see providerForModel).
		rt.provider = r.provider
		rt.providers = nil
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
