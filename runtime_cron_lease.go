package ovr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

// Cron leader-leases give at-most-once firing when N replicas share one state
// backend. Each cron plan elects a leader through a fenced TTL lease on the
// shared state.LeaseStore; followers poll for takeover, the leader runs the
// timer loop and renews in the background plus synchronously before every
// fire, and a per-fire idempotency reservation closes the residual window
// where two replicas could both believe they own the same tick.
const (
	// cronLeaseTTL is fixed by design; there is intentionally no knob.
	// Tests shorten it through cronLeaseConfig, never through env.
	cronLeaseTTL = 30 * time.Second
	// cronLeaseFollowerPoll is the follower AcquireLease cadence before
	// ±20% jitter is applied.
	cronLeaseFollowerPoll = 15 * time.Second
	// cronLeaseReleaseTimeout bounds the best-effort ReleaseLease issued on
	// graceful shutdown, after the run context is already cancelled.
	cronLeaseReleaseTimeout = 5 * time.Second
)

// cronLeaseConfig carries the lease machinery for one runtime. A nil config
// means unleased v0.2 behavior.
type cronLeaseConfig struct {
	leases state.LeaseStore
	holder string
	ttl    time.Duration
	poll   time.Duration
}

// cronLeaseStamp identifies the leadership lease under which one cron fire
// runs; emitPipelineEvent stamps it into the fire's event payloads.
type cronLeaseStamp struct {
	name   string
	holder string
	fence  uint64
}

// cronLeaseConfigForRuntime decides whether cron leader-leases are active:
// the state store must implement state.LeaseStore and the
// OUVRIER_CRON_LEASE=off escape hatch must not be set. A nil return restores
// the unleased v0.2 loop exactly.
func cronLeaseConfigForRuntime(rt httpRuntime) *cronLeaseConfig {
	if rt.stateStore == nil {
		return nil
	}
	leases, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envnames.CronLease)), "off") {
		return nil
	}
	return &cronLeaseConfig{
		leases: leases,
		holder: cronReplicaID(),
		ttl:    cronLeaseTTL,
		poll:   cronLeaseFollowerPoll,
	}
}

// processCronReplicaSuffix is the per-process rand8 of the default
// <hostname>-<rand8> holder identity, generated once so every lease this
// process touches shares one holder.
var processCronReplicaSuffix = sync.OnceValue(func() string {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%08x", os.Getpid())
	}
	return hex.EncodeToString(raw[:])
})

// cronReplicaID returns this replica's lease holder identity:
// OUVRIER_REPLICA_ID when set, otherwise <hostname>-<rand8>.
func cronReplicaID() string {
	if id := strings.TrimSpace(os.Getenv(envnames.ReplicaID)); id != "" {
		return id
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "replica"
	}
	return host + "-" + processCronReplicaSuffix()
}

// cronLeaseNamesForPlans names one lease per plan as
// cron:<sha256-8 of trigger expr>:<occurrence-index among same-expr plans>,
// so names survive unrelated plan reordering.
func cronLeaseNamesForPlans(plans []runtimeplan.Plan) []string {
	occurrences := make(map[string]int, len(plans))
	names := make([]string, len(plans))
	for i, plan := range plans {
		expr := plan.Trigger.Expr
		sum := sha256.Sum256([]byte(expr))
		names[i] = fmt.Sprintf("cron:%x:%d", sum[:4], occurrences[expr])
		occurrences[expr]++
	}
	return names
}

// cronFireKey is the per-tick idempotency key shared by every replica:
// identical lease name plus identical scheduledAt dedupes a tick globally.
// RFC3339Nano renders whole seconds exactly like RFC3339 and keeps
// sub-second @every intervals collision-free.
func cronFireKey(leaseName string, scheduledAt time.Time) string {
	return "cron_fire:" + leaseName + ":" + scheduledAt.UTC().Format(time.RFC3339Nano)
}

// epochAlignedCronSchedule fires @every intervals on epoch multiples of the
// interval instead of relative to the local clock, so scheduledAt (and with
// it the per-fire idempotency key) matches across replicas.
type epochAlignedCronSchedule struct {
	interval time.Duration
}

func (s epochAlignedCronSchedule) Next(after time.Time) time.Time {
	period := int64(s.interval)
	elapsed := after.UnixNano()
	slot := elapsed / period
	if elapsed < 0 && elapsed%period != 0 {
		slot-- // floor division for pre-epoch instants
	}
	return time.Unix(0, (slot+1)*period).UTC()
}

// leaseAlignedCronSchedule rewrites local-now-relative @every schedules to
// their epoch-aligned form; field cron schedules are already absolute.
func leaseAlignedCronSchedule(schedule cronSchedule) cronSchedule {
	if interval, ok := schedule.(intervalCronSchedule); ok {
		return epochAlignedCronSchedule(interval)
	}
	return schedule
}

// runCronPlansWithLeaseConfig runs one cron loop per plan: the unleased v0.2
// loop when cfg is nil, the follower/leader lease state machine otherwise.
func runCronPlansWithLeaseConfig(ctx context.Context, rt httpRuntime, plans []runtimeplan.Plan, schedules []cronSchedule, cfg *cronLeaseConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	var leaseNames []string
	if cfg != nil {
		leaseNames = cronLeaseNamesForPlans(plans)
	}
	var wg sync.WaitGroup
	for i, plan := range plans {
		wg.Add(1)
		if cfg == nil {
			go func(plan runtimeplan.Plan, schedule cronSchedule) {
				defer wg.Done()
				runCronLoop(ctx, rt, plan, schedule)
			}(plan, schedules[i])
			continue
		}
		go func(plan runtimeplan.Plan, schedule cronSchedule, leaseName string) {
			defer wg.Done()
			runLeasedCronLoop(ctx, rt, plan, leaseAlignedCronSchedule(schedule), leaseName, *cfg)
		}(plan, schedules[i], leaseNames[i])
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// runLeasedCronLoop is the follower side of the state machine: poll
// AcquireLease with jitter until this replica wins, then hand the existing
// timer loop to runCronLeaderLoop until leadership is lost or the runtime
// shuts down.
func runLeasedCronLoop(ctx context.Context, rt httpRuntime, plan runtimeplan.Plan, schedule cronSchedule, leaseName string, cfg cronLeaseConfig) {
	for ctx.Err() == nil {
		lease, acquired, err := cfg.leases.AcquireLease(ctx, leaseName, cfg.holder, cfg.ttl)
		if err != nil || !acquired {
			if !sleepCronFollowerPoll(ctx, cfg.poll) {
				return
			}
			continue
		}
		emitCronLeaseEvent(ctx, rt, events.EventCronLeaseAcquired, cronLeasePayload(leaseName, cfg.holder, lease))
		runCronLeaderLoop(ctx, rt, plan, schedule, leaseName, cfg, lease)
	}
}

// sleepCronFollowerPoll waits one follower poll interval with ±20% jitter so
// contending replicas do not stampede the lease row. It reports false when
// the context ended first.
func sleepCronFollowerPoll(ctx context.Context, poll time.Duration) bool {
	jittered := poll
	if spread := int64(poll) * 2 / 5; spread > 0 { // 0.4 * poll
		jittered = poll - time.Duration(spread/2) + time.Duration(mathrand.Int64N(spread+1))
	}
	timer := time.NewTimer(jittered)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// runCronLeaderLoop is the leader side: the v0.2 timer loop plus a
// background renewer at TTL/3, a synchronous renew immediately before each
// fire, and a per-tick idempotency reservation. It returns when leadership is
// lost (drop back to follower) or the context ends (release the lease for
// instant failover). In-flight runs are never cancelled on leadership loss;
// missed ticks are never backfilled.
func runCronLeaderLoop(ctx context.Context, rt httpRuntime, plan runtimeplan.Plan, schedule cronSchedule, leaseName string, cfg cronLeaseConfig, lease state.Lease) {
	renewCtx, stopRenewer := context.WithCancel(ctx)
	defer stopRenewer()

	// The fence is constant for the whole leadership term: renewals never
	// change it, only a takeover does — and a takeover ends this term. The
	// renewer goroutine therefore shares the immutable fence, never the
	// lease value the timer loop keeps refreshing.
	fence := lease.Fence
	lost := make(chan struct{})
	renewerDone := make(chan struct{})
	go func() {
		defer close(renewerDone)
		ticker := time.NewTicker(cfg.ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if _, renewed, err := cfg.leases.RenewLease(renewCtx, leaseName, cfg.holder, fence, cfg.ttl); err != nil || !renewed {
					if renewCtx.Err() == nil {
						close(lost)
					}
					return
				}
			}
		}
	}()

	workerPool := newCronWorkerPool(plan.Trigger.WorkerPool)
	var wg sync.WaitGroup
	defer wg.Wait()

	shutdown := func() {
		// Stop the renewer before releasing so a racing renewal cannot
		// resurrect the tombstoned lease with our still-valid fence.
		stopRenewer()
		<-renewerDone
		releaseCronLeaseOnShutdown(cfg, leaseName, lease)
	}

	for {
		next := schedule.Next(time.Now())
		if next.IsZero() {
			// No future fire exists; hold leadership until shutdown or loss
			// so a second replica does not flap on a dead schedule.
			select {
			case <-ctx.Done():
				shutdown()
				return
			case <-lost:
				emitCronLeaseEvent(ctx, rt, events.EventCronLeaseLost, cronLeasePayload(leaseName, cfg.holder, lease))
				return
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			shutdown()
			return
		case <-lost:
			timer.Stop()
			emitCronLeaseEvent(ctx, rt, events.EventCronLeaseLost, cronLeasePayload(leaseName, cfg.holder, lease))
			return
		case <-timer.C:
			// Synchronous renew immediately before the fire: failure means
			// skip the fire, emit cron_lease_lost, drop to follower.
			renewed, ok, err := cfg.leases.RenewLease(ctx, leaseName, cfg.holder, lease.Fence, cfg.ttl)
			if err != nil || !ok {
				if ctx.Err() != nil {
					shutdown()
					return
				}
				emitCronLeaseEvent(ctx, rt, events.EventCronLeaseLost, cronLeasePayload(leaseName, cfg.holder, lease))
				return
			}
			lease = renewed

			session, err := newHTTPPipelineSession(plan)
			if err != nil {
				continue
			}
			_, reserved, err := rt.stateStore.ReserveIdempotency(ctx, cronFireKey(leaseName, next), session.ExecID)
			if err != nil || !reserved {
				payload := cronLeasePayload(leaseName, cfg.holder, lease)
				payload["scheduled_at"] = next.UTC().Format(time.RFC3339Nano)
				if err != nil {
					payload["reason"] = "reserve_error"
					payload["error"] = err.Error()
				} else {
					payload["reason"] = "duplicate_fire"
				}
				emitCronLeaseEvent(ctx, rt, events.EventCronTickSkipped, payload)
				continue
			}
			if !acquireCronWorker(ctx, workerPool) {
				shutdown()
				return
			}
			wg.Add(1)
			go func(scheduledAt time.Time, session runtimeplan.Session, stamp cronLeaseStamp) {
				defer wg.Done()
				defer releaseCronWorker(workerPool)
				fireRT := rt
				fireRT.cronLease = &stamp
				_, _ = runCronPlanOnceWithSession(ctx, fireRT, plan, scheduledAt, &session)
			}(next, session, cronLeaseStamp{name: leaseName, holder: cfg.holder, fence: lease.Fence})
		}
	}
}

// releaseCronLeaseOnShutdown tombstones the lease on graceful shutdown so the
// next replica takes over immediately instead of waiting out the TTL. The run
// context is already cancelled, so it runs on its own short deadline;
// failures are ignored — the TTL remains the backstop.
func releaseCronLeaseOnShutdown(cfg cronLeaseConfig, leaseName string, lease state.Lease) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), cronLeaseReleaseTimeout)
	defer cancel()
	_ = cfg.leases.ReleaseLease(releaseCtx, leaseName, cfg.holder, lease.Fence)
}

func cronLeasePayload(leaseName, holder string, lease state.Lease) map[string]any {
	return map[string]any{
		"lease":      leaseName,
		"holder":     holder,
		"fence":      lease.Fence,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

// emitCronLeaseEvent records a lease lifecycle event; emission failures never
// interfere with scheduling.
func emitCronLeaseEvent(ctx context.Context, rt httpRuntime, kind events.EventKind, payload map[string]any) {
	_ = rt.emitRuntimeEvent(ctx, planRunResult{}, kind, payload)
}
