package ovr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// Durable-run recovery (#40), the read side of the journal written in
// durable_runs.go. While a journaled run executes, its owner heartbeats the
// fenced lease run:<exec_id> on the shared state.LeaseStore (same machinery
// as cron leader-leases); the recovery loop scans the journal with jitter,
// claims only expired leases, and applies the replay policy:
//
//   - plan_key missing or plan_hash mismatch (pipeline edited between
//     deploys) => clean EventRunAbandoned, journal pruned;
//   - sync reply/SSE terminal => EventRunAbandoned (the client is gone);
//   - interrupted step with an open tool intent, or a completed
//     side-effecting intent (no dedup exists for those) =>
//     EventReplayIndeterminateTool, run marked failed, journal kept for the
//     operator to inspect via GET /admin/runs?status=orphaned and force via
//     POST /admin/runs/{execID}/recover;
//   - otherwise replay from the last checkpoint: at-least-once for read-only
//     work, with idempotent calls deduped by the same-exec ReserveIdempotency
//     interpretation in internal/tools/idempotency.go.

// durableRunLeaseName names the fenced lease that guards one durable run.
func durableRunLeaseName(execID string) string {
	return "run:" + execID
}

var errDurableRunLeaseLost = errors.New("durable run lease lost")

// durableRunLeaseHeartbeat owns one held run lease: a TTL/3 renewer keeps it
// alive while the run (or its recovery replay) executes. Its execution
// context is cancelled with cause as soon as renewal cannot prove ownership;
// providers, tools, and terminals must all run under that context. release()
// stops renewal and tombstones the lease for instant takeover. Renewals use a
// separate control context so a cancelled request cannot strand an unexpired
// lease until the TTL backstop.
type durableRunLeaseHeartbeat struct {
	leases          state.LeaseStore
	name            string
	holder          string
	ttl             time.Duration
	lease           state.Lease
	executionCtx    context.Context
	cancelExecution context.CancelCauseFunc
	stop            context.CancelFunc
	done            chan struct{}
	stopOnce        sync.Once
	renewMu         sync.Mutex
	lossOnce        sync.Once
	lossMu          sync.RWMutex
	lossErr         error
}

func startDurableRunLeaseHeartbeat(parent context.Context, leases state.LeaseStore, name, holder string, ttl time.Duration, lease state.Lease) *durableRunLeaseHeartbeat {
	if parent == nil {
		parent = context.Background()
	}
	executionCtx, cancelExecution := context.WithCancelCause(parent)
	renewCtx, stop := context.WithCancel(context.Background())
	heartbeat := &durableRunLeaseHeartbeat{
		leases:          leases,
		name:            name,
		holder:          holder,
		ttl:             ttl,
		lease:           lease,
		executionCtx:    executionCtx,
		cancelExecution: cancelExecution,
		stop:            stop,
		done:            make(chan struct{}),
	}
	go func() {
		defer close(heartbeat.done)
		interval := ttl / 3
		if interval <= 0 {
			interval = time.Nanosecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if err := heartbeat.renew(renewCtx); err != nil {
					if renewCtx.Err() != nil {
						return
					}
					heartbeat.markLost(err)
					return
				}
			}
		}
	}()
	return heartbeat
}

func (h *durableRunLeaseHeartbeat) renew(ctx context.Context) error {
	h.renewMu.Lock()
	defer h.renewMu.Unlock()
	current, renewed, err := h.leases.RenewLease(ctx, h.name, h.holder, h.lease.Fence, h.ttl)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: renew %s fence %d: %w", errDurableRunLeaseLost, h.name, h.lease.Fence, err)
	}
	if !renewed {
		if current.Name == "" {
			return fmt.Errorf("%w: %s fence %d is no longer owned", errDurableRunLeaseLost, h.name, h.lease.Fence)
		}
		return fmt.Errorf("%w: %s fence %d is now held by %s at fence %d", errDurableRunLeaseLost, h.name, h.lease.Fence, current.Holder, current.Fence)
	}
	return nil
}

func (h *durableRunLeaseHeartbeat) markLost(err error) {
	if h == nil || err == nil {
		return
	}
	h.lossOnce.Do(func() {
		h.lossMu.Lock()
		h.lossErr = err
		h.lossMu.Unlock()
		h.cancelExecution(err)
	})
}

func (h *durableRunLeaseHeartbeat) loss() error {
	if h == nil {
		return nil
	}
	h.lossMu.RLock()
	defer h.lossMu.RUnlock()
	return h.lossErr
}

func (h *durableRunLeaseHeartbeat) context() context.Context {
	if h == nil || h.executionCtx == nil {
		return context.Background()
	}
	return h.executionCtx
}

// executionError preserves the operation error and adds a cancellation-shaped
// lease-loss cause. The cancellation component makes lifecycle persistence use
// an uncancelled cleanup context, while the sentinel keeps the real reason
// inspectable with errors.Is.
func (h *durableRunLeaseHeartbeat) executionError(operationErr error) error {
	loss := h.loss()
	if loss == nil {
		return operationErr
	}
	if operationErr == nil {
		return errors.Join(context.Canceled, loss)
	}
	return errors.Join(operationErr, context.Canceled, loss)
}

// confirm proves that holder+fence are still current immediately before a
// terminal or completion boundary. Any store error is fail-closed: inability
// to prove ownership is indistinguishable from ownership loss.
func (h *durableRunLeaseHeartbeat) confirm(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if err := h.executionError(nil); err != nil {
		return err
	}
	if err := h.renew(ctx); err != nil {
		if errors.Is(err, errDurableRunLeaseLost) {
			h.markLost(err)
			return h.executionError(nil)
		}
		return err
	}
	return nil
}

func (h *durableRunLeaseHeartbeat) stopRenewing() {
	if h == nil {
		return
	}
	h.stopOnce.Do(func() {
		h.stop()
		<-h.done
	})
}

// stopAndConfirm freezes the renewal goroutine, then extends the still-owned
// lease once synchronously. Completion and journal pruning can therefore run
// inside a fresh TTL window with no late heartbeat racing the decision.
func (h *durableRunLeaseHeartbeat) stopAndConfirm(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.stopRenewing()
	return h.confirm(ctx)
}

// release stops the renewer and tombstones the lease so the journal row stops
// looking live the moment its run finishes or parks. After a takeover the
// fence no longer matches and the release is a harmless no-op.
func (h *durableRunLeaseHeartbeat) release() {
	if h == nil {
		return
	}
	h.stopRenewing()
	h.cancelExecution(context.Canceled)
	releaseCtx, cancel := context.WithTimeout(context.Background(), cronLeaseReleaseTimeout)
	defer cancel()
	_ = h.leases.ReleaseLease(releaseCtx, h.name, h.holder, h.lease.Fence)
}

// durableRunLeaseTTLForRuntime resolves the run-lease TTL with the fixed
// default as backstop.
func (rt httpRuntime) durableRunLeaseTTLForRuntime() time.Duration {
	if rt.durableRuns != nil && rt.durableRuns.leaseTTL > 0 {
		return rt.durableRuns.leaseTTL
	}
	return durableRunLeaseTTL
}

// acquireDurableRunLease claims run:<execID> and starts its heartbeat. It
// returns (nil, nil) when the store has no lease capability — journal-only
// durability — and an error when the lease is held by another live holder,
// which means someone else owns this run right now.
func (rt httpRuntime) acquireDurableRunLease(ctx context.Context, execID string) (*durableRunLeaseHeartbeat, error) {
	if rt.durableRuns == nil || rt.stateStore == nil {
		return nil, nil
	}
	leases, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		return nil, nil
	}
	name := durableRunLeaseName(execID)
	holder := cronReplicaID()
	ttl := rt.durableRunLeaseTTLForRuntime()
	lease, acquired, err := leases.AcquireLease(ctx, name, holder, ttl)
	if err != nil {
		return nil, fmt.Errorf("acquire durable run lease %s: %w", name, err)
	}
	if !acquired {
		return nil, fmt.Errorf("durable run lease %s is held by %s until %s", name, lease.Holder, lease.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return startDurableRunLeaseHeartbeat(ctx, leases, name, holder, ttl, lease), nil
}

// acquireDurableRunLeaseWithRetry retries acquisition briefly for the resumed
// leg of a suspended run: the suspend path's deferred release may still be in
// flight when the operator approves. A lease that stays held past the retry
// budget belongs to a live holder and the caller must not proceed.
func (rt httpRuntime) acquireDurableRunLeaseWithRetry(ctx context.Context, execID string) (*durableRunLeaseHeartbeat, error) {
	const (
		attempts = 40
		backoff  = 50 * time.Millisecond
	)
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		heartbeat, err := rt.acquireDurableRunLease(ctx, execID)
		if err == nil {
			return heartbeat, nil
		}
		lastErr = err
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

// startDurableRunRecovery launches the periodic recovery loop on the
// runtime's async group when recovery is configured and the store can hold
// leases. plans are the runtime's compiled plans, used to re-resolve each
// journal row's pipeline by plan_key and verify its plan_hash.
func startDurableRunRecovery(rt httpRuntime, plans []runtimeplan.Plan) {
	if rt.durableRuns == nil || rt.durableRuns.recovery == nil || rt.stateStore == nil {
		return
	}
	leases, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		return
	}
	recoveryPlans := append([]runtimeplan.Plan(nil), plans...)
	rt.startAsync(func(ctx context.Context) {
		rt.runDurableRecoveryLoop(ctx, leases, recoveryPlans)
	})
}

// runDurableRecoveryLoop scans on startup and then every scan period with
// ±20% jitter (the cron follower-poll pattern) until the runtime shuts down.
func (rt httpRuntime) runDurableRecoveryLoop(ctx context.Context, leases state.LeaseStore, plans []runtimeplan.Plan) {
	cfg := rt.durableRuns.recovery
	scan := cfg.scan
	if scan <= 0 {
		scan = durableRecoveryScan
	}
	concurrency := cfg.concurrency
	if concurrency <= 0 {
		concurrency = durableRecoveryConcurrency
	}
	pool := newCronWorkerPool(concurrency)
	// The event-stream sync is a full events scan; running it on every cycle
	// is wasted work because handler startup already synced and recovery is
	// this stream's only other writer. The first scan re-syncs once to cover
	// runtimes wired without the handler path, then later cycles skip it.
	syncEvents := true
	for ctx.Err() == nil {
		rt.recoverDurableRunsScan(ctx, leases, plans, pool, syncEvents)
		syncEvents = false
		if !sleepCronFollowerPoll(ctx, scan) {
			return
		}
	}
}

// recoverDurableRunsScan performs one synchronous journal scan: claimable
// rows are claimed via fenced lease takeover and replayed under the worker
// pool; the scan returns once every replay it dispatched has finished.
// syncEvents asks for the (full-scan) event-stream ID sync first; the
// periodic loop requests it only on its first cycle.
func (rt httpRuntime) recoverDurableRunsScan(ctx context.Context, leases state.LeaseStore, plans []runtimeplan.Plan, pool chan struct{}, syncEvents bool) {
	if syncEvents {
		_ = rt.syncEventStreamWithStore(ctx)
	}
	journals, err := rt.stateStore.RunJournals(ctx)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, journal := range journals {
		if ctx.Err() != nil {
			return
		}
		execution, claimable, err := rt.durableRunClaimable(ctx, journal)
		if err != nil || !claimable {
			continue
		}
		if !acquireCronWorker(ctx, pool) {
			return
		}
		// AcquireLease succeeds only when the run lease is absent, expired,
		// or tombstoned: a slow-but-alive run heartbeating its lease is never
		// stolen, and the fence takeover invalidates any zombie renewer.
		lease, acquired, err := leases.AcquireLease(ctx, durableRunLeaseName(journal.ExecID), cronReplicaID(), rt.durableRunLeaseTTLForRuntime())
		if err != nil || !acquired {
			releaseCronWorker(pool)
			continue
		}
		wg.Add(1)
		go func(journal state.RunJournal, execution state.Execution, lease state.Lease) {
			defer wg.Done()
			defer releaseCronWorker(pool)
			rt.recoverClaimedDurableRun(ctx, leases, plans, journal, execution, lease, false)
		}(journal, execution, lease)
	}
}

// durableRunClaimable reports whether a journal row is an interrupted run the
// automatic loop may claim: its execution is still recorded as running (a
// failed run — indeterminate or denied — is operator territory, a completed
// one is awaiting retention) and it is not parked on a pending approval.
func (rt httpRuntime) durableRunClaimable(ctx context.Context, journal state.RunJournal) (state.Execution, bool, error) {
	execution, ok, err := rt.stateStore.Execution(ctx, journal.ExecID)
	if err != nil || !ok {
		return state.Execution{}, false, err
	}
	if execution.Status != state.ExecutionRunning {
		return execution, false, nil
	}
	parked, err := rt.durableRunHasPendingApproval(ctx, journal.ExecID)
	if err != nil || parked {
		// Suspended awaiting a human decision: not orphaned, replaying it
		// would mint a duplicate approval. The scan picks the run up once the
		// record is approved (cross-restart approval resume).
		return execution, false, err
	}
	return execution, true, nil
}

// durableRunHasPendingApproval reports whether the run is parked on a pending
// human approval. Both the automatic scan and the operator-forced
// POST /admin/runs/{execID}/recover must refuse such a run: replaying it
// would mint a duplicate approval.
func (rt httpRuntime) durableRunHasPendingApproval(ctx context.Context, execID string) (bool, error) {
	approvals, err := rt.stateStore.ApprovalsForExecution(ctx, execID)
	if err != nil {
		return false, err
	}
	for _, approval := range approvals {
		if approval.Status == state.ApprovalPending {
			return true, nil
		}
	}
	return false, nil
}

// recoverClaimedDurableRun applies the replay policy to one claimed run while
// heartbeating its lease. force is the operator override from
// POST /admin/runs/{execID}/recover: it skips the tool-intent fail-loud gate
// (the operator has inspected the intents) but never the plan-identity or
// client-gone checks.
func (rt httpRuntime) recoverClaimedDurableRun(ctx context.Context, leases state.LeaseStore, plans []runtimeplan.Plan, journal state.RunJournal, execution state.Execution, lease state.Lease, force bool) {
	heartbeat := startDurableRunLeaseHeartbeat(ctx, leases, durableRunLeaseName(journal.ExecID), cronReplicaID(), rt.durableRunLeaseTTLForRuntime(), lease)
	executionCtx := heartbeat.context()
	defer func() {
		if heartbeat.loss() != nil {
			finishCtx, cancel := context.WithTimeout(context.Background(), cronLeaseReleaseTimeout)
			rt.failDurableRunExecution(finishCtx, execution)
			cancel()
		}
		heartbeat.release()
	}()

	// Re-check pending approvals now that the lease is ours: an operator
	// approving in the scan→claim gap parks the run on a fresh pending record,
	// and replaying it here would mint a duplicate approval. On pending (or a
	// store error that leaves it unknown) skip the replay; the deferred
	// release tombstones the claim so the approval resume — or the next scan —
	// takes over instantly.
	if pending, err := rt.durableRunHasPendingApproval(executionCtx, journal.ExecID); err != nil || pending {
		return
	}

	plan, ok := durablePlanForJournal(plans, journal)
	if !ok {
		rt.abandonDurableRun(executionCtx, journal, execution, "plan_missing")
		return
	}
	if durablePlanHash(plan) != journal.PlanHash {
		rt.abandonDurableRun(executionCtx, journal, execution, "plan_hash_mismatch")
		return
	}
	if durableRunClientGone(plan) {
		rt.abandonDurableRun(executionCtx, journal, execution, "client_gone")
		return
	}

	checkpoints, err := rt.stateStore.RunCheckpoints(executionCtx, journal.ExecID)
	if err != nil {
		return
	}
	resumeIndex, input, replayUnsafe, replaySource, replayStep := durableReplayInput(journal, checkpoints)
	if replayUnsafe {
		rt.markDurableRunReplayUnsafe(executionCtx, journal, execution, replaySource, replayStep)
		return
	}
	if !force {
		intents, err := rt.stateStore.ToolIntents(executionCtx, journal.ExecID)
		if err != nil {
			return
		}
		if blocking, blocked := durableReplayBlockingIntent(intents, resumeIndex); blocked {
			rt.markDurableRunIndeterminate(executionCtx, journal, execution, blocking)
			return
		}
	}
	rt.replayDurableRun(executionCtx, plan, journal, execution, resumeIndex, input, force, heartbeat)
}

func durableReplayInput(journal state.RunJournal, checkpoints []state.RunCheckpoint) (resumeIndex int, input string, replayUnsafe bool, source string, stepIndex int) {
	resumeIndex = 0
	input = journal.Input
	replayUnsafe = journal.ReplayUnsafe
	source = "journal_input"
	stepIndex = -1
	if len(checkpoints) == 0 {
		return
	}
	last := checkpoints[len(checkpoints)-1]
	return last.StepIndex + 1, last.Output, last.ReplayUnsafe, "checkpoint_output", last.StepIndex
}

// markDurableRunReplayUnsafe refuses a replay whose required input was
// credential-redacted before persistence. Replaying the placeholder would
// silently corrupt business behavior. The journal is deliberately retained
// for inspection; unlike an edited/missing plan, no forced replay can recover
// the original secret-bearing value.
func (rt httpRuntime) markDurableRunReplayUnsafe(ctx context.Context, journal state.RunJournal, execution state.Execution, source string, stepIndex int) {
	payload := map[string]any{
		"plan_key": journal.PlanKey,
		"reason":   "replay_input_redacted",
		"source":   source,
	}
	if stepIndex >= 0 {
		payload["step_index"] = stepIndex
	}
	rt.emitDurableRecoveryEvent(ctx, journal, execution, events.EventRunAbandoned, payload)
	rt.failDurableRunExecution(ctx, execution)
}

// durablePlanForJournal re-resolves a journal row against the CURRENT
// compiled plans: prefer an exact plan_key + plan_hash match (covers two
// plans sharing one key), otherwise the first plan_key match so the caller's
// hash check reports the mismatch.
func durablePlanForJournal(plans []runtimeplan.Plan, journal state.RunJournal) (runtimeplan.Plan, bool) {
	var keyMatch runtimeplan.Plan
	found := false
	for _, plan := range plans {
		if durablePlanKey(plan) != journal.PlanKey {
			continue
		}
		if durablePlanHash(plan) == journal.PlanHash {
			return plan, true
		}
		if !found {
			keyMatch = plan
			found = true
		}
	}
	return keyMatch, found
}

// durableRunClientGone reports whether the plan's terminal needs a live
// client connection: synchronous Reply (SSE included) cannot deliver to a
// caller that disappeared with the crashed process. Async replies, Push, and
// Sink terminals deliver out-of-band and stay recoverable.
func durableRunClientGone(plan runtimeplan.Plan) bool {
	return plan.Terminal.Kind == runtimeplan.TerminalReply && (plan.Terminal.SSE || !plan.Terminal.Async)
}

// durableReplayBlockingIntent applies the side-effect replay policy to the
// interrupted steps (every intent at or after resumeIndex; intents on
// checkpointed steps are already covered by the checkpoint):
//
//   - an OPEN intent means the crash hit mid-call and the outcome is
//     indeterminate — never auto-replay;
//   - a COMPLETED side_effecting intent means the effect happened and has no
//     dedup, so replaying the step would duplicate it — never auto-replay;
//   - a COMPLETED idempotent intent is safe: its ReserveIdempotency key is
//     held by this exec_id and re-execution is idempotent by contract.
func durableReplayBlockingIntent(intents []state.ToolIntent, resumeIndex int) (state.ToolIntent, bool) {
	for _, intent := range intents {
		if intent.StepIndex < resumeIndex {
			continue
		}
		if intent.CompletedAt.IsZero() {
			return intent, true
		}
		if intent.Effect != string(policy.EffectIdempotent) {
			return intent, true
		}
	}
	return state.ToolIntent{}, false
}

// markDurableRunIndeterminate fails the run loudly and keeps its journal so
// the operator can inspect the intents and force a replay.
func (rt httpRuntime) markDurableRunIndeterminate(ctx context.Context, journal state.RunJournal, execution state.Execution, intent state.ToolIntent) {
	rt.emitDurableRecoveryEvent(ctx, journal, execution, events.EventReplayIndeterminateTool, map[string]any{
		"plan_key":     journal.PlanKey,
		"tool":         intent.ToolName,
		"tool_call_id": intent.ToolCallID,
		"step_index":   intent.StepIndex,
		"effect":       intent.Effect,
		"idem_key":     intent.IdemKey,
		"open":         intent.CompletedAt.IsZero(),
	})
	rt.failDurableRunExecution(ctx, execution)
}

// abandonDurableRun cleanly ends a run recovery cannot replay: event, failed
// execution, journal pruned.
func (rt httpRuntime) abandonDurableRun(ctx context.Context, journal state.RunJournal, execution state.Execution, reason string) {
	rt.emitDurableRecoveryEvent(ctx, journal, execution, events.EventRunAbandoned, map[string]any{
		"plan_key": journal.PlanKey,
		"reason":   reason,
	})
	rt.failDurableRunExecution(ctx, execution)
	if err := rt.stateStore.PruneRunJournal(ctx, journal.ExecID); err != nil {
		rt.durableRuns.health.recordPruneFailure(err)
	}
}

func (rt httpRuntime) failDurableRunExecution(ctx context.Context, execution state.Execution) {
	if current, ok, err := rt.stateStore.Execution(ctx, execution.ExecID); err == nil && ok {
		execution = current
	}
	if execution.Status != state.ExecutionRunning {
		return
	}
	execution.Status = state.ExecutionFailed
	execution.CompletedAt = time.Now().UTC()
	_ = rt.stateStore.SaveExecution(ctx, execution)
}

// replayDurableRun re-enters execution from the last checkpoint: the
// remaining steps run through the regular step loop with the durable journal
// offset to the original plan indices (the same withBase machinery the
// approval resume uses), then the terminal applies and the run finishes —
// pruning the journal on success.
func (rt httpRuntime) replayDurableRun(ctx context.Context, plan runtimeplan.Plan, journal state.RunJournal, execution state.Execution, resumeIndex int, input string, forced bool, heartbeat *durableRunLeaseHeartbeat) {
	session, err := runtimeplan.NewSession(httpPipelineSessionModel(plan),
		runtimeplan.WithSessionIDs(journal.ExecID, "", execution.TraceID),
		runtimeplan.WithSessionBudget(runtimeplan.Budget{
			MaxIterations: harness.DefaultMaxIterations,
			MaxTokens:     harness.DefaultMaxTokens,
			MaxCostUSD:    harness.DefaultMaxCostUSD,
			MaxWallClock:  harness.DefaultMaxWallClock,
		}))
	if err != nil {
		return
	}
	if forced {
		// The operator is replaying a run already marked failed; flip it back
		// to running so the finish below records the replay's real outcome.
		execution.Status = state.ExecutionRunning
		execution.CompletedAt = time.Time{}
		if err := rt.stateStore.SaveExecution(ctx, execution); err != nil {
			return
		}
	}
	if err := rt.stateStore.SaveSession(ctx, session); err != nil {
		return
	}
	rt.emitDurableRecoveryEvent(ctx, journal, execution, events.EventRunRecovered, map[string]any{
		"plan_key":          journal.PlanKey,
		"trigger":           journal.TriggerKind,
		"resumed_from_step": resumeIndex,
		"forced":            forced,
	})

	replayCtx := rt.durableApprovalReplayContext(ctx, journal.ExecID)
	// Known v0.3 limitation: the replay starts with a fresh budget ledger (no
	// budgetLedger on the scope) and the default session budgets above, so
	// cross-step budget progress accrued before the crash — tokens, cost,
	// iterations, wall clock — is not restored. A replayed run can therefore
	// spend up to a full budget again on its remaining steps.
	scope := planRunScope{
		parentSession:        &session,
		durable:              &durableStepJournal{store: rt.stateStore, execID: journal.ExecID, baseIndex: resumeIndex},
		idempotencyNamespace: toolIdempotencyPlanNamespace(plan),
		idempotencyBase:      resumeIndex,
	}
	result := planRunResult{Output: input, Session: session, HasSession: true}
	if resumeIndex < len(plan.Steps) {
		remaining := append([]runtimeplan.Step(nil), plan.Steps[resumeIndex:]...)
		stepResult, err := rt.runStepsResult(replayCtx, remaining, input, scope)
		if !stepResult.HasSession {
			stepResult.Session = session
			stepResult.HasSession = true
		}
		if err != nil {
			err = heartbeat.executionError(err)
			if !errors.Is(err, errDurableRunLeaseLost) && rt.rememberSuspendedPlan(err, plan, session) {
				// Re-suspended on a fresh gated call: the run parks again with
				// its journal intact; the deferred lease release lets a later
				// approval (or recovery after another crash) pick it up.
				return
			}
			_ = rt.finishPipelineExecution(context.WithoutCancel(ctx), session, plan, "failed", err)
			return
		}
		result = stepResult
	}
	if err := heartbeat.confirm(replayCtx); err != nil {
		err = heartbeat.executionError(err)
		_ = rt.finishPipelineExecution(context.WithoutCancel(ctx), session, plan, "failed", err)
		return
	}
	terminalJournal := &durableStepJournal{store: rt.stateStore, execID: journal.ExecID}
	terminalCtx := terminalJournal.intentContext(replayCtx, len(plan.Steps))
	if err := rt.applyResumedTerminal(terminalCtx, plan, result); err != nil {
		err = heartbeat.executionError(err)
		_ = rt.finishPipelineExecution(context.WithoutCancel(ctx), session, plan, "failed", err)
		return
	}
	settleCtx, settleCancel := newHTTPFinalizationContext(replayCtx)
	_ = rt.resolveTriggerIdempotency(settleCtx, session.ExecID, "completed")
	settleCancel()
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(replayCtx), cronLeaseReleaseTimeout)
	err = heartbeat.stopAndConfirm(confirmCtx)
	cancel()
	if err != nil {
		err = heartbeat.executionError(err)
		_ = rt.finishPipelineExecutionWithTriggerOutcome(context.WithoutCancel(ctx), session, plan, "failed", "completed", err)
		return
	}
	_ = rt.finishPipelineExecution(context.WithoutCancel(ctx), session, plan, "completed", nil)
}

// durableApprovalReplayContext installs the cross-restart approval fallback:
// a gated tool call replayed under this context is auto-allowed when an
// approval record for this execution is already approved and its args_hash
// matches the replayed call exactly. The in-memory resume registry stays the
// fast path for live runs; this resolver only exists inside recovery replays.
//
// The approvals are loaded once here: approved records are immutable for the
// lifetime of the replay, so every gated call scans the cached slice instead
// of re-querying the store. If the load fails the resolver always misses —
// gated calls re-park on a fresh approval rather than silently treating a
// transient store error as "no approvals" — and one log line records the
// fallback.
func (rt httpRuntime) durableApprovalReplayContext(ctx context.Context, execID string) context.Context {
	ctx = tools.ContextWithIdempotencyReplay(ctx)
	approvals, err := rt.stateStore.ApprovalsForExecution(ctx, execID)
	if err != nil {
		log.Printf("ouvrier: durable recovery exec_id=%s: loading approvals for replay failed (%v); replayed gated calls will re-park on fresh approvals", execID, err)
		approvals = nil
	}
	return tools.ContextWithApprovedApprovalResolver(ctx, func(_ context.Context, toolName, argsHash string) (string, bool) {
		if argsHash == "" {
			return "", false
		}
		for _, approval := range approvals {
			if approval.Status != state.ApprovalApproved {
				continue
			}
			if approval.ToolName == toolName && approval.ArgsHash != "" && approval.ArgsHash == argsHash {
				return approval.ID, true
			}
		}
		return "", false
	})
}

// emitDurableRecoveryEvent records one recovery lifecycle event against the
// recovered execution's identifiers; emission failures never block recovery.
func (rt httpRuntime) emitDurableRecoveryEvent(ctx context.Context, journal state.RunJournal, execution state.Execution, kind events.EventKind, payload map[string]any) {
	// Skip only when BOTH sinks are absent (nothing to write to); with either
	// one present we still emit — appendRuntimeEvent handles each nil sink on
	// its own. This is deliberately &&, not ||.
	if rt.stateStore == nil && rt.eventStream == nil {
		return
	}
	_ = rt.appendRuntimeEvent(ctx, events.Event{
		Kind:    kind,
		ExecID:  journal.ExecID,
		TraceID: execution.TraceID,
		Payload: payload,
	})
}
