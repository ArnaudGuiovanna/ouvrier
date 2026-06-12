package ovr

import (
	"context"
	"crypto/sha256"
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
// bracketing every non-read tool call. This file is the write side only —
// recovery, run leases, and the /admin/runs endpoints are out of scope.

// defaultDurableRetention bounds how long failed/suspended run journals are
// kept before the retention sweep prunes them.
const defaultDurableRetention = 72 * time.Hour

// durableRunsConfig carries the durable-run settings for one runtime. A nil
// config means the feature is off: zero journal writes, no behavior change.
type durableRunsConfig struct {
	retention time.Duration
	health    *durableRunsHealth
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
	return &durableRunsConfig{retention: retention, health: &durableRunsHealth{}}
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
	return newDurableRunsConfig(retention), nil
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

// durablePlanHash fingerprints the compiled steps so recovery can detect a
// pipeline edited between deploys (plan_hash mismatch => abandon, #40). It
// hashes the structural identity of every step — kinds, goals, models,
// tool/bash/skill/MCP/subagent names and effects, nested branches — never
// secrets, schemas' Go types, or function pointers.
func durablePlanHash(steps []runtimeplan.Step) string {
	hash := sha256.New()
	writePlanHashSteps(hash, steps, 0)
	return hex.EncodeToString(hash.Sum(nil))
}

func writePlanHashSteps(hash io.Writer, steps []runtimeplan.Step, depth int) {
	for index, step := range steps {
		fmt.Fprintf(hash, "%d/%d|%s|%s|%s|%v|%d|%t|%t|%t\n",
			depth, index, step.Kind, step.Goal, step.Model, step.Fallback,
			step.Concurrency, step.PartialOK, step.NoCache, step.SequentialTools)
		for _, tool := range step.Tools {
			fmt.Fprintf(hash, "tool|%s|%s|%s|%v|%t\n",
				tool.Name, tool.Effect, tool.IdempotencyKey, tool.SideEffects, tool.RequiresApproval)
		}
		for _, bash := range step.Bash {
			fmt.Fprintf(hash, "bash|%s|%t\n", bash.Name, bash.UnsafeHostExecution)
		}
		for _, skill := range step.Skills {
			fmt.Fprintf(hash, "skill|%s\n", skill.Name)
		}
		for _, server := range step.MCPServers {
			fmt.Fprintf(hash, "mcp|%s\n", server.Name)
		}
		for _, subAgent := range step.SubAgents {
			fmt.Fprintf(hash, "subagent|%s|%d|%t\n", subAgent.Name, subAgent.MaxParallel, subAgent.PartialOK)
			writePlanHashSteps(hash, subAgent.Pipeline.Steps, depth+1)
		}
		if step.ResultSchema != nil {
			fmt.Fprintf(hash, "schema|%s\n", step.ResultSchema.Name)
		}
		for _, branch := range step.Branches {
			fmt.Fprintf(hash, "branch\n")
			writePlanHashSteps(hash, branch.Steps, depth+1)
		}
		if len(step.MapPipeline.Steps) > 0 {
			fmt.Fprintf(hash, "map\n")
			writePlanHashSteps(hash, step.MapPipeline.Steps, depth+1)
		}
	}
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
