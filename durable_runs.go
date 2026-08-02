package ovr

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// Durable runs (OUVRIER_DURABLE_RUNS=1, default off) journal each opted-in
// pipeline execution so a crashed worker leaves enough state behind for
// recovery (#40): one run_journal row at start, one checkpoint per completed
// top-level pipe step (Parallel/Map as one unit), and a tool_intent row
// bracketing every non-read tool call. This file holds the write side and the
// configuration; the read side — run leases, the recovery loop, and the
// /admin/runs endpoints — lives in durable_recovery.go.

// defaultDurableRetention bounds how long terminal failed run journals are
// kept before the retention sweep prunes them. Running runs and executions
// parked on a pending approval are never retention candidates.
const defaultDurableRetention = 72 * time.Hour

const (
	// durableRunLeaseTTL is fixed by design, matching the cron lease TTL;
	// tests shorten it through durableRunsConfig, never through env.
	durableRunLeaseTTL = 30 * time.Second
	// durableRecoveryScan is the recovery scan cadence before ±20% jitter.
	durableRecoveryScan = 30 * time.Second
	// durableRecoveryConcurrency caps concurrent run replays per replica.
	durableRecoveryConcurrency = 2
)

// durableRunsConfig carries the durable-run settings for one runtime. A nil
// config means the feature is off: zero journal writes, no behavior change.
type durableRunsConfig struct {
	retention time.Duration
	health    *durableRunsHealth
	// leaseTTL is the run-lease TTL heartbeated while a journaled run
	// executes and claimed by recovery once expired.
	leaseTTL time.Duration
	// recovery enables the periodic recovery scan; nil keeps the journal
	// write side (and lease heartbeats) without ever replaying runs, which is
	// what most unit tests want.
	recovery *durableRecoveryConfig
}

// durableRecoveryConfig tunes the recovery loop, mirroring cronLeaseConfig's
// test-injectable shape.
type durableRecoveryConfig struct {
	// scan is the journal scan period before ±20% jitter.
	scan time.Duration
	// concurrency caps simultaneous replays (cron worker-pool pattern).
	concurrency int
}

func newDurableRecoveryConfig() *durableRecoveryConfig {
	return &durableRecoveryConfig{scan: durableRecoveryScan, concurrency: durableRecoveryConcurrency}
}

// durableRunsHealth aggregates prune failures for /admin/health. It is
// shared by every copy of the owning httpRuntime value.
type durableRunsHealth struct {
	mu               sync.Mutex
	pruneFailures    uint64
	lastPruneError   string
	lastPruneErrorAt time.Time
}

func (h *durableRunsHealth) recordPruneFailure(err error) {
	if h == nil || err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneFailures++
	h.lastPruneError = err.Error()
	h.lastPruneErrorAt = time.Now().UTC()
}

func (h *durableRunsHealth) snapshot() (uint64, string, time.Time) {
	if h == nil {
		return 0, "", time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pruneFailures, h.lastPruneError, h.lastPruneErrorAt
}

func newDurableRunsConfig(retention time.Duration) *durableRunsConfig {
	if retention <= 0 {
		retention = defaultDurableRetention
	}
	return &durableRunsConfig{
		retention: retention,
		health:    &durableRunsHealth{},
		leaseTTL:  durableRunLeaseTTL,
	}
}

// durableRunsEnabledFromEnv parses the OUVRIER_DURABLE_RUNS opt-in flag.
// Unset and explicit falsy values mean off; unrecognized values are a
// startup error rather than a silently disabled journal.
func durableRunsEnabledFromEnv() (bool, error) {
	raw := strings.TrimSpace(os.Getenv(envnames.DurableRuns))
	if raw == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (e.g. 1 or 0), got %q", envnames.DurableRuns, raw)
	}
	return enabled, nil
}

// durableRetentionFromEnv parses OUVRIER_DURABLE_RETENTION (default 72h).
func durableRetentionFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(envnames.DurableRetention))
	if raw == "" {
		return defaultDurableRetention, nil
	}
	retention, err := time.ParseDuration(raw)
	if err != nil || retention <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration (e.g. 72h), got %q", envnames.DurableRetention, raw)
	}
	return retention, nil
}

// durableRunsConfigForStore resolves the durable-runs startup configuration
// for the given state store. With the flag off it returns (nil, nil); with
// the flag on it refuses backends that cannot persist a run journal across a
// process crash — the in-memory backend and custom public StateStore
// implementations — with an actionable error.
func durableRunsConfigForStore(store state.Store) (*durableRunsConfig, error) {
	enabled, err := durableRunsEnabledFromEnv()
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	retention, err := durableRetentionFromEnv()
	if err != nil {
		return nil, err
	}
	switch store.(type) {
	case *state.MemoryStore:
		return nil, fmt.Errorf("%s=1 requires a durable state backend: %s=%s cannot persist run journals across a restart; set %s=%s or %s=%s",
			envnames.DurableRuns, envnames.StateBackend, state.BackendMemory,
			envnames.StateBackend, state.BackendSQLite, envnames.StateBackend, state.BackendPostgres)
	case publicStateStoreAdapter:
		return nil, fmt.Errorf("%s=1 is not supported with a custom WithStateStore: the run journal needs the built-in %s or %s state backend",
			envnames.DurableRuns, state.BackendSQLite, state.BackendPostgres)
	}
	if _, err := durableReplayBuildIdentity(); err != nil {
		return nil, fmt.Errorf("%s=1 cannot bind recovery to the running worker executable: %w", envnames.DurableRuns, err)
	}
	config := newDurableRunsConfig(retention)
	// Env-configured runtimes recover interrupted runs; test runtimes opt in
	// by setting recovery explicitly.
	config.recovery = newDurableRecoveryConfig()
	return config, nil
}

type durableBuildIdentityResult struct {
	identity string
	err      error
}

var (
	durableBuildIdentityOnce   sync.Once
	durableBuildIdentityCached durableBuildIdentityResult
)

// durableReplayBuildIdentity fingerprints the exact executable image once per
// process. The Linux /proc handle stays bound to the running image even when a
// deploy atomically replaces its pathname; os.Executable is the portable
// fallback. Production durable mode refuses to start when neither image can be
// read, because a structure-only plan hash could replay changed Go handlers.
func durableReplayBuildIdentity() (string, error) {
	durableBuildIdentityOnce.Do(func() {
		identity, err := fingerprintDurableExecutable()
		if err == nil {
			durableBuildIdentityCached.identity = identity
			return
		}

		// Keep internal/manual test configurations fail-closed too. Production
		// startup returns the error above; the per-process fallback merely makes
		// hashes from an unreadable executable unable to match after a restart.
		var nonce [32]byte
		if _, randomErr := cryptorand.Read(nonce[:]); randomErr == nil {
			durableBuildIdentityCached.identity = "unavailable:" + hex.EncodeToString(nonce[:])
		} else {
			durableBuildIdentityCached.identity = fmt.Sprintf("unavailable:%d:%d", os.Getpid(), time.Now().UnixNano())
		}
		durableBuildIdentityCached.err = err
	})
	return durableBuildIdentityCached.identity, durableBuildIdentityCached.err
}

func fingerprintDurableExecutable() (string, error) {
	paths := []string{"/proc/self/exe"}
	executable, executableErr := os.Executable()
	if executableErr == nil && executable != "" && executable != paths[0] {
		paths = append(paths, executable)
	}

	var lastErr error
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			lastErr = err
			continue
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			lastErr = copyErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
	}
	if executableErr != nil {
		lastErr = executableErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no executable path is available")
	}
	return "", fmt.Errorf("fingerprint current executable: %w", lastErr)
}

// durablePlanKey identifies a plan by its trigger, the same identity an
// operator sees in /admin/plans: kind plus the trigger's distinguishing
// detail.
func durablePlanKey(plan runtimeplan.Plan) string {
	trigger := plan.Trigger
	switch trigger.Kind {
	case runtimeplan.TriggerHTTP, runtimeplan.TriggerWebhook:
		return fmt.Sprintf("%s:%s %s", trigger.Kind, trigger.Method, trigger.Path)
	case runtimeplan.TriggerCron:
		return fmt.Sprintf("%s:%s", trigger.Kind, trigger.Expr)
	case runtimeplan.TriggerStream:
		return fmt.Sprintf("%s:%s", trigger.Kind, streamDisplayURI(trigger.URI))
	default:
		return string(trigger.Kind)
	}
}

// durablePlanHash fingerprints the complete replay contract so recovery can
// detect a worker edited between deploys (plan_hash mismatch => abandon,
// #40). In particular, terminal destinations, schemas, and the exact worker
// executable are part of the contract: an interrupted run must never resume
// against changed Go handler/tool code or a newly configured sink or queue.
// Length-prefixed fields avoid delimiter collisions. Function pointers and
// secret values loaded from the environment are never serialized; configured
// URIs and the executable identity remain confined to the digest.
func durablePlanHash(plan runtimeplan.Plan) string {
	buildIdentity, _ := durableReplayBuildIdentity()
	return durablePlanHashWithBuildIdentity(plan, buildIdentity)
}

func durablePlanHashWithBuildIdentity(plan runtimeplan.Plan, buildIdentity string) string {
	hash := sha256.New()
	writePlanHashFields(hash, "ouvrier-durable-plan", "v3", "worker-build", buildIdentity)
	writePlanHashTrigger(hash, plan.Trigger)
	writePlanHashSteps(hash, plan.Steps, 0)
	writePlanHashTerminal(hash, plan.Terminal)
	return hex.EncodeToString(hash.Sum(nil))
}

func writePlanHashSteps(hash io.Writer, steps []runtimeplan.Step, depth int) {
	writePlanHashFields(hash, "steps", strconv.Itoa(depth), strconv.Itoa(len(steps)))
	for index, step := range steps {
		writePlanHashFields(hash, "step", strconv.Itoa(depth), strconv.Itoa(index),
			string(step.Kind), step.Goal, step.Model,
			strconv.Itoa(step.Concurrency), strconv.FormatBool(step.PartialOK),
			strconv.FormatBool(step.NoCache), strconv.FormatBool(step.SequentialTools))
		writePlanHashStrings(hash, "fallback", step.Fallback)

		writePlanHashFields(hash, "tools", strconv.Itoa(len(step.Tools)))
		for _, tool := range step.Tools {
			writePlanHashFields(hash, "tool", tool.Name, tool.Description,
				string(tool.InputSchema), tool.ArgumentName, string(tool.Effect),
				tool.IdempotencyKey, strconv.FormatBool(tool.RequiresApproval),
				strconv.FormatInt(int64(tool.Timeout), 10))
			writePlanHashStrings(hash, "side-effects", tool.SideEffects)
		}

		writePlanHashFields(hash, "bash-tools", strconv.Itoa(len(step.Bash)))
		for _, bash := range step.Bash {
			writePlanHashFields(hash, "bash", bash.Name, bash.SandboxRoot,
				strconv.FormatInt(int64(bash.Timeout), 10), strconv.Itoa(bash.MaxOutputBytes),
				strconv.FormatBool(bash.UnsafeHostExecution))
			writePlanHashStrings(hash, "allowed-env", bash.AllowedEnv)
		}

		writePlanHashFields(hash, "skills", strconv.Itoa(len(step.Skills)))
		for _, skill := range step.Skills {
			writePlanHashFields(hash, "skill", skill.Name)
		}
		writePlanHashFields(hash, "mcp-servers", strconv.Itoa(len(step.MCPServers)))
		for _, server := range step.MCPServers {
			writePlanHashFields(hash, "mcp", server.Name)
		}
		writePlanHashFields(hash, "subagents", strconv.Itoa(len(step.SubAgents)))
		for _, subAgent := range step.SubAgents {
			writePlanHashFields(hash, "subagent", subAgent.Name,
				strconv.Itoa(subAgent.MaxParallel), strconv.FormatBool(subAgent.PartialOK))
			writePlanHashSteps(hash, subAgent.Pipeline.Steps, depth+1)
		}

		writePlanHashResultSchema(hash, step.ResultSchema)
		if step.Retry == nil {
			writePlanHashFields(hash, "retry", "absent")
		} else {
			writePlanHashFields(hash, "retry", "present",
				strconv.Itoa(step.Retry.ProviderRetries),
				strconv.FormatInt(int64(step.Retry.Backoff), 10))
		}
		writePlanHashFields(hash, "budget",
			strconv.Itoa(step.Budget.MaxIterations), strconv.Itoa(step.Budget.MaxTokens),
			strconv.FormatFloat(step.Budget.MaxCostUSD, 'g', -1, 64),
			strconv.FormatInt(int64(step.Budget.MaxWallClock), 10))

		writePlanHashFields(hash, "branches", strconv.Itoa(len(step.Branches)))
		for _, branch := range step.Branches {
			writePlanHashFields(hash, "branch")
			writePlanHashSteps(hash, branch.Steps, depth+1)
		}
		writePlanHashFields(hash, "map")
		writePlanHashSteps(hash, step.MapPipeline.Steps, depth+1)
	}
}

func writePlanHashTrigger(hash io.Writer, trigger runtimeplan.Trigger) {
	writePlanHashFields(hash, "trigger", string(trigger.Kind), trigger.Method,
		trigger.Path, trigger.Expr, trigger.Value, trigger.URI,
		strconv.Itoa(trigger.WorkerPool), trigger.IdempotencyHeader,
		trigger.SignatureEnv, trigger.SignatureHeader, trigger.DLQTarget,
		strconv.Itoa(trigger.MaxAttempts), strconv.Itoa(trigger.MaxInFlight),
		trigger.AckPolicy)
}

func writePlanHashTerminal(hash io.Writer, terminal runtimeplan.Terminal) {
	writePlanHashFields(hash, "terminal", string(terminal.Kind),
		strconv.FormatBool(terminal.Async), strconv.FormatBool(terminal.SSE),
		strconv.FormatBool(terminal.SinkLog), terminal.SinkFilePath,
		terminal.PushWebhookURL, terminal.PushQueueURI)
	writePlanHashResultSchema(hash, terminal.ResultSchema)
}

func writePlanHashResultSchema(hash io.Writer, schema *runtimeplan.ResultSchema) {
	if schema == nil {
		writePlanHashFields(hash, "schema", "absent")
		return
	}
	typePackage, typeName := "", ""
	if schema.Type != nil {
		typePackage = schema.Type.PkgPath()
		typeName = schema.Type.String()
	}
	writePlanHashFields(hash, "schema", "present", schema.Name,
		typePackage, typeName, string(schema.JSONSchema))
}

func writePlanHashStrings(hash io.Writer, label string, values []string) {
	writePlanHashFields(hash, label, strconv.Itoa(len(values)))
	for _, value := range values {
		writePlanHashFields(hash, value)
	}
}

func writePlanHashFields(hash io.Writer, fields ...string) {
	var size [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = io.WriteString(hash, field)
	}
}

// toolIdempotencyPlanNamespace and its child/step derivations scope an
// idempotent business key to the exact logical Pipe definition that owns the
// tool. They intentionally omit executions and sessions so deduplication
// remains durable across runs, while same-named tools in different Pipes can
// never reserve each other's keys.
func toolIdempotencyPlanNamespace(plan runtimeplan.Plan) string {
	hash := sha256.New()
	writePlanHashFields(hash, "ouvrier-tool-idempotency", "v1", durablePlanKey(plan))
	return "pipe:" + hex.EncodeToString(hash.Sum(nil))
}

func toolIdempotencyStepNamespace(parent string, index int, step runtimeplan.Step) string {
	hash := sha256.New()
	writePlanHashFields(hash, "ouvrier-tool-idempotency-step", "v1", parent, strconv.Itoa(index))
	writePlanHashSteps(hash, []runtimeplan.Step{step}, 0)
	return "pipe:" + hex.EncodeToString(hash.Sum(nil))
}

func toolIdempotencyChildNamespace(parent, kind string, index int) string {
	hash := sha256.New()
	writePlanHashFields(hash, "ouvrier-tool-idempotency-child", "v1", parent, kind, strconv.Itoa(index))
	return "pipe:" + hex.EncodeToString(hash.Sum(nil))
}

// durableStepJournal is the per-run checkpoint writer threaded through
// planRunScope. baseIndex offsets step indices when a suspended run resumes
// with only its remaining steps.
type durableStepJournal struct {
	store     state.Store
	execID    string
	baseIndex int
}

func (j *durableStepJournal) checkpoint(ctx context.Context, stepIndex int, output string) error {
	if j == nil || j.store == nil {
		return nil
	}
	if err := j.store.SaveRunCheckpoint(ctx, state.RunCheckpoint{
		ExecID:    j.execID,
		StepIndex: j.baseIndex + stepIndex,
		Output:    output,
	}); err != nil {
		return fmt.Errorf("durable run checkpoint (step %d): %w", j.baseIndex+stepIndex, err)
	}
	return nil
}

// withBase returns a copy whose step indices start after the given local
// step, for resuming the steps that follow a suspended one.
func (j *durableStepJournal) withBase(localSteps int) *durableStepJournal {
	if j == nil {
		return nil
	}
	return &durableStepJournal{store: j.store, execID: j.execID, baseIndex: j.baseIndex + localSteps}
}

// intentContext installs the tool intent recorder for one top-level step so
// every non-read tool executed under it (branches and subagents included) is
// bracketed by an intent row carrying this step's index.
func (j *durableStepJournal) intentContext(ctx context.Context, stepIndex int) context.Context {
	if j == nil || j.store == nil {
		return ctx
	}
	return tools.ContextWithToolIntentRecorder(ctx, stateToolIntentRecorder{store: j.store}, j.execID, j.baseIndex+stepIndex)
}

// stateToolIntentRecorder bridges the durable state.Store to the
// tools.ToolIntentRecorder seam.
type stateToolIntentRecorder struct {
	store state.Store
}

func (r stateToolIntentRecorder) BeginToolIntent(ctx context.Context, intent tools.ToolIntent) error {
	return r.store.BeginToolIntent(ctx, state.ToolIntent{
		ExecID:     intent.ExecID,
		ToolCallID: intent.ToolCallID,
		StepIndex:  intent.StepIndex,
		ToolName:   intent.ToolName,
		Effect:     intent.Effect,
		IdemKey:    intent.IdemKey,
	})
}

func (r stateToolIntentRecorder) CompleteToolIntent(ctx context.Context, execID, toolCallID string) error {
	return r.store.CompleteToolIntent(ctx, execID, toolCallID)
}
