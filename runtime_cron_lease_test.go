package ovr

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestEpochAlignedCronScheduleAlignsToEpochMultiples(t *testing.T) {
	schedule := epochAlignedCronSchedule{interval: 10 * time.Second}

	next := schedule.Next(time.Date(2026, 5, 21, 6, 0, 3, 0, time.UTC))
	want := time.Date(2026, 5, 21, 6, 0, 10, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("Next = %s, want %s", next, want)
	}

	// An instant exactly on a multiple schedules the strictly-next multiple.
	next = schedule.Next(want)
	if !next.Equal(want.Add(10 * time.Second)) {
		t.Fatalf("Next on boundary = %s, want %s", next, want.Add(10*time.Second))
	}
}

func TestEpochAlignedCronScheduleIsIdenticalAcrossReplicas(t *testing.T) {
	// Two replicas observe different local clocks within the same interval
	// slot; both must compute the same scheduledAt for the next fire.
	schedule := epochAlignedCronSchedule{interval: 15 * time.Second}
	replicaA := schedule.Next(time.Date(2026, 5, 21, 6, 0, 1, 0, time.UTC))
	replicaB := schedule.Next(time.Date(2026, 5, 21, 6, 0, 14, 999_999_999, time.UTC))
	if !replicaA.Equal(replicaB) {
		t.Fatalf("replica scheduledAt diverged: %s vs %s", replicaA, replicaB)
	}
	if replicaA.UnixNano()%int64(15*time.Second) != 0 {
		t.Fatalf("scheduledAt %s is not an epoch multiple of the interval", replicaA)
	}
}

func TestEpochAlignedCronScheduleSubSecondIntervals(t *testing.T) {
	schedule := epochAlignedCronSchedule{interval: 250 * time.Millisecond}
	after := time.Date(2026, 5, 21, 6, 0, 0, 100_000_000, time.UTC)
	next := schedule.Next(after)
	if !next.After(after) {
		t.Fatalf("Next = %s, not after %s", next, after)
	}
	if next.UnixNano()%int64(250*time.Millisecond) != 0 {
		t.Fatalf("Next %s is not an epoch multiple of 250ms", next)
	}
}

func TestLeaseAlignedCronScheduleOnlyRewritesIntervalSchedules(t *testing.T) {
	interval, err := parseCronSchedule("@every 10s")
	if err != nil {
		t.Fatalf("parseCronSchedule returned error: %v", err)
	}
	if _, ok := leaseAlignedCronSchedule(interval).(epochAlignedCronSchedule); !ok {
		t.Fatalf("leaseAlignedCronSchedule(@every) = %T, want epochAlignedCronSchedule", leaseAlignedCronSchedule(interval))
	}

	field, err := parseCronSchedule("0 6 * * *")
	if err != nil {
		t.Fatalf("parseCronSchedule returned error: %v", err)
	}
	aligned := leaseAlignedCronSchedule(field)
	if _, ok := aligned.(fieldCronSchedule); !ok {
		t.Fatalf("leaseAlignedCronSchedule(field cron) = %T, want untouched fieldCronSchedule", aligned)
	}
}

func TestCronLeaseNamesAreStableUnderUnrelatedReordering(t *testing.T) {
	planFor := func(expr string) runtimeplan.Plan {
		return runtimeplan.Plan{Trigger: runtimeplan.Trigger{Kind: runtimeplan.TriggerCron, Expr: expr}}
	}

	names := cronLeaseNamesForPlans([]runtimeplan.Plan{
		planFor("@every 5m"),
		planFor("0 6 * * *"),
		planFor("@every 5m"),
	})
	for _, name := range names {
		if !strings.HasPrefix(name, "cron:") {
			t.Fatalf("lease name %q missing cron: prefix", name)
		}
		parts := strings.Split(name, ":")
		if len(parts) != 3 || len(parts[1]) != 8 {
			t.Fatalf("lease name %q is not cron:<sha256-8>:<occurrence>", name)
		}
	}
	if names[0] == names[2] {
		t.Fatalf("same-expr plans share lease name %q", names[0])
	}
	if names[0][:len(names[0])-1] != names[2][:len(names[2])-1] {
		t.Fatalf("same-expr plans differ beyond occurrence index: %q vs %q", names[0], names[2])
	}

	// Removing the unrelated field-cron plan must not rename the @every leases.
	reordered := cronLeaseNamesForPlans([]runtimeplan.Plan{
		planFor("@every 5m"),
		planFor("@every 5m"),
	})
	if reordered[0] != names[0] || reordered[1] != names[2] {
		t.Fatalf("lease names changed under unrelated reordering: %v vs %v", reordered, []string{names[0], names[2]})
	}
}

func TestCronLeaseConfigForRuntimeGating(t *testing.T) {
	if cfg := cronLeaseConfigForRuntime(httpRuntime{}); cfg != nil {
		t.Fatalf("config for nil store = %+v, want nil", cfg)
	}

	store := state.NewMemoryStore()
	cfg := cronLeaseConfigForRuntime(httpRuntime{stateStore: store})
	if cfg == nil {
		t.Fatal("config for LeaseStore-capable store = nil, want enabled")
	}
	if cfg.ttl != 30*time.Second {
		t.Fatalf("ttl = %s, want fixed 30s", cfg.ttl)
	}
	if cfg.poll != 15*time.Second {
		t.Fatalf("poll = %s, want 15s", cfg.poll)
	}

	t.Setenv("OUVRIER_REPLICA_ID", "replica-from-env")
	cfg = cronLeaseConfigForRuntime(httpRuntime{stateStore: store})
	if cfg == nil || cfg.holder != "replica-from-env" {
		t.Fatalf("holder = %+v, want OUVRIER_REPLICA_ID override", cfg)
	}

	t.Setenv("OUVRIER_CRON_LEASE", "off")
	if cfg := cronLeaseConfigForRuntime(httpRuntime{stateStore: store}); cfg != nil {
		t.Fatalf("config with OUVRIER_CRON_LEASE=off = %+v, want nil", cfg)
	}
}

// leaselessStateStore hides the LeaseStore capability of a wrapped store so
// the v0.2 fallback path can be asserted.
type leaselessStateStore struct {
	state.Store
}

func TestCronLeaseConfigForRuntimeRequiresLeaseStore(t *testing.T) {
	store := leaselessStateStore{Store: state.NewMemoryStore()}
	if cfg := cronLeaseConfigForRuntime(httpRuntime{stateStore: store}); cfg != nil {
		t.Fatalf("config for non-LeaseStore backend = %+v, want nil", cfg)
	}
}

func TestCronReplicaIDDefaultsToHostnameRand8(t *testing.T) {
	t.Setenv("OUVRIER_REPLICA_ID", "")
	id := cronReplicaID()
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "replica"
	}
	if !strings.HasPrefix(id, host+"-") {
		t.Fatalf("replica id = %q, want %q prefix", id, host+"-")
	}
	suffix := strings.TrimPrefix(id, host+"-")
	if len(suffix) != 8 {
		t.Fatalf("replica id suffix = %q, want 8 hex chars", suffix)
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		t.Fatalf("replica id suffix %q is not hex: %v", suffix, err)
	}
	if again := cronReplicaID(); again != id {
		t.Fatalf("replica id changed within one process: %q vs %q", again, id)
	}
}

func TestCronFireKeyFormat(t *testing.T) {
	at := time.Date(2026, 5, 21, 6, 0, 10, 0, time.UTC)
	key := cronFireKey("cron:ab12cd34:0", at)
	if key != "cron_fire:cron:ab12cd34:0:2026-05-21T06:00:10Z" {
		t.Fatalf("cronFireKey = %q", key)
	}
}

// --- integration helpers -----------------------------------------------

// cronFireRecorder is a webhook target shared by the simulated replicas. The
// runtimes in the two-replica tests are built without an EventStream so two
// stream cursors never race on shared event IDs; fires are observed through
// the Push(Webhook) terminal instead, which also carries the epoch-aligned
// scheduled_at of every fire.
type cronFireRecorder struct {
	mu    sync.Mutex
	fires []string
}

func (r *cronFireRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		var payload struct {
			ScheduledAt string `json:"scheduled_at"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.fires = append(r.fires, payload.ScheduledAt)
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}
}

func (r *cronFireRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.fires...)
}

func (r *cronFireRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.fires)
}

func newLeasedCronRuntime(t *testing.T, store state.Store) httpRuntime {
	t.Helper()
	return httpRuntime{stateStore: store, toolExecutor: outputAllowedExecutor("webhook")}
}

func openSharedSQLiteStore(t *testing.T, path string) *state.SQLiteStore {
	t.Helper()
	store, err := state.NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore(%q) returned error: %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func compileCronWebhookPlans(t *testing.T, expr, webhookURL string) ([]runtimeplan.Plan, []cronSchedule) {
	t.Helper()
	plans, err := compilePlans([]Node{
		From(Cron(expr)),
		Push(Webhook(webhookURL)),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	schedules, err := cronSchedulesForPlans(plans)
	if err != nil {
		t.Fatalf("cronSchedulesForPlans returned error: %v", err)
	}
	return plans, schedules
}

func startLeasedCronReplica(ctx context.Context, rt httpRuntime, plans []runtimeplan.Plan, schedules []cronSchedule, cfg cronLeaseConfig) chan error {
	done := make(chan error, 1)
	go func() {
		done <- runCronPlansWithLeaseConfig(ctx, rt, plans, schedules, &cfg)
	}()
	return done
}

func waitForCondition(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func assertDistinctFires(t *testing.T, fires []string) {
	t.Helper()
	seen := make(map[string]int, len(fires))
	for _, fire := range fires {
		seen[fire]++
	}
	for scheduledAt, count := range seen {
		if count > 1 {
			t.Fatalf("tick %s fired %d times, want exactly once (all fires: %v)", scheduledAt, count, fires)
		}
	}
}

// --- two-replica integration tests --------------------------------------

func TestLeasedCronTwoRuntimesFireEachTickExactlyOnce(t *testing.T) {
	recorder := &cronFireRecorder{}
	webhook := httptest.NewServer(recorder.handler())
	defer webhook.Close()

	path := filepath.Join(t.TempDir(), "state.db")
	storeA := openSharedSQLiteStore(t, path)
	storeB := openSharedSQLiteStore(t, path)

	plans, schedules := compileCronWebhookPlans(t, "@every 200ms", webhook.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfgA := cronLeaseConfig{leases: storeA, holder: "replica-a", ttl: 2 * time.Second, poll: 100 * time.Millisecond}
	cfgB := cronLeaseConfig{leases: storeB, holder: "replica-b", ttl: 2 * time.Second, poll: 100 * time.Millisecond}
	doneA := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, storeA), plans, schedules, cfgA)
	doneB := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, storeB), plans, schedules, cfgB)

	waitForCondition(t, 10*time.Second, "at least four cron fires", func() bool {
		return recorder.count() >= 4
	})
	cancel()
	for _, done := range []chan error{doneA, doneB} {
		if err := <-done; err != nil {
			t.Fatalf("replica loop returned error: %v", err)
		}
	}

	fires := recorder.snapshot()
	if len(fires) < 4 {
		t.Fatalf("fires = %d, want at least 4", len(fires))
	}
	assertDistinctFires(t, fires)
	for _, fire := range fires {
		at, err := time.Parse(time.RFC3339Nano, fire)
		if err != nil {
			t.Fatalf("scheduled_at %q is not RFC3339: %v", fire, err)
		}
		if at.UnixNano()%int64(200*time.Millisecond) != 0 {
			t.Fatalf("scheduled_at %s is not epoch-aligned to the interval", at)
		}
	}
}

func TestLeasedCronDuplicateReplicaIDDoesNotDoubleFire(t *testing.T) {
	recorder := &cronFireRecorder{}
	webhook := httptest.NewServer(recorder.handler())
	defer webhook.Close()

	path := filepath.Join(t.TempDir(), "state.db")
	storeA := openSharedSQLiteStore(t, path)
	storeB := openSharedSQLiteStore(t, path)

	plans, schedules := compileCronWebhookPlans(t, "@every 200ms", webhook.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Both replicas run with the same (duplicated) OUVRIER_REPLICA_ID-style
	// holder. There is no holder self-reacquire path and fences differ on
	// every takeover, so ticks must still fire exactly once.
	cfg := cronLeaseConfig{leases: storeA, holder: "duplicated-id", ttl: 2 * time.Second, poll: 100 * time.Millisecond}
	cfgB := cfg
	cfgB.leases = storeB
	doneA := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, storeA), plans, schedules, cfg)
	doneB := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, storeB), plans, schedules, cfgB)

	waitForCondition(t, 10*time.Second, "at least four cron fires", func() bool {
		return recorder.count() >= 4
	})
	cancel()
	<-doneA
	<-doneB

	assertDistinctFires(t, recorder.snapshot())
}

func TestLeasedCronFollowerTakesOverExpiredLeader(t *testing.T) {
	recorder := &cronFireRecorder{}
	webhook := httptest.NewServer(recorder.handler())
	defer webhook.Close()

	path := filepath.Join(t.TempDir(), "state.db")
	store := openSharedSQLiteStore(t, path)

	plans, schedules := compileCronWebhookPlans(t, "@every 150ms", webhook.URL)
	leaseName := cronLeaseNamesForPlans(plans)[0]

	// Simulate a leader killed without releasing: its lease row simply
	// expires. The follower must take over within TTL + poll jitter.
	if _, acquired, err := store.AcquireLease(context.Background(), leaseName, "dead-replica", 400*time.Millisecond); err != nil || !acquired {
		t.Fatalf("seed AcquireLease acquired=%v err=%v", acquired, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cronLeaseConfig{leases: store, holder: "replica-b", ttl: time.Second, poll: 100 * time.Millisecond}
	done := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, store), plans, schedules, cfg)

	// Budget: 400ms seed TTL + 120ms max jittered poll + scheduling slack.
	waitForCondition(t, 5*time.Second, "follower to take over and fire", func() bool {
		return recorder.count() >= 1
	})
	leases, err := store.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	if len(leases) != 1 || leases[0].Holder != "replica-b" || leases[0].Fence != 2 {
		t.Fatalf("lease after takeover = %+v, want replica-b at fence 2", leases)
	}
	cancel()
	<-done
}

func TestLeasedCronReleaseOnShutdownHandsOverImmediately(t *testing.T) {
	recorder := &cronFireRecorder{}
	webhook := httptest.NewServer(recorder.handler())
	defer webhook.Close()

	path := filepath.Join(t.TempDir(), "state.db")
	storeA := openSharedSQLiteStore(t, path)
	storeB := openSharedSQLiteStore(t, path)

	plans, schedules := compileCronWebhookPlans(t, "@every 150ms", webhook.URL)

	leaderCtx, stopLeader := context.WithCancel(context.Background())
	defer stopLeader()
	// TTL far above the assertion budget: a handover can only come from the
	// shutdown ReleaseLease, never from expiry.
	cfgA := cronLeaseConfig{leases: storeA, holder: "replica-a", ttl: 30 * time.Second, poll: 100 * time.Millisecond}
	doneA := startLeasedCronReplica(leaderCtx, newLeasedCronRuntime(t, storeA), plans, schedules, cfgA)

	holderOf := func() (string, uint64) {
		leases, err := storeB.Leases(context.Background())
		if err != nil || len(leases) == 0 {
			return "", 0
		}
		return leases[0].Holder, leases[0].Fence
	}
	waitForCondition(t, 5*time.Second, "replica-a to take the lease", func() bool {
		holder, _ := holderOf()
		return holder == "replica-a"
	})

	followerCtx, stopFollower := context.WithCancel(context.Background())
	defer stopFollower()
	cfgB := cronLeaseConfig{leases: storeB, holder: "replica-b", ttl: 30 * time.Second, poll: 100 * time.Millisecond}
	doneB := startLeasedCronReplica(followerCtx, newLeasedCronRuntime(t, storeB), plans, schedules, cfgB)

	// Graceful shutdown of the leader releases the lease...
	stopLeader()
	if err := <-doneA; err != nil {
		t.Fatalf("leader loop returned error: %v", err)
	}
	// ...so the follower acquires at the next poll, far below the 30s TTL.
	start := time.Now()
	waitForCondition(t, 3*time.Second, "replica-b to take over after release", func() bool {
		holder, fence := holderOf()
		return holder == "replica-b" && fence == 2
	})
	if handover := time.Since(start); handover >= 5*time.Second {
		t.Fatalf("handover took %s, want well under the 30s TTL", handover)
	}
	waitForCondition(t, 5*time.Second, "replica-b to fire", func() bool {
		return recorder.count() >= 1
	})
	stopFollower()
	<-doneB
}

func TestLeasedCronEmitsLeaseEventsAndStampsFence(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	rt := httpRuntime{stateStore: store, eventStream: stream}

	plans, err := compilePlans([]Node{
		From(Cron("@every 100ms")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	schedules, err := cronSchedulesForPlans(plans)
	if err != nil {
		t.Fatalf("cronSchedulesForPlans returned error: %v", err)
	}
	leaseName := cronLeaseNamesForPlans(plans)[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := cronLeaseConfig{leases: store, holder: "replica-a", ttl: time.Second, poll: 50 * time.Millisecond}
	done := startLeasedCronReplica(ctx, rt, plans, schedules, cfg)

	eventsOfKind := func(kind events.EventKind) []events.Event {
		recorded, err := store.EventsSince(context.Background(), "", 0)
		if err != nil {
			t.Fatalf("EventsSince returned error: %v", err)
		}
		var matched []events.Event
		for _, event := range recorded {
			if event.Kind == kind {
				matched = append(matched, event)
			}
		}
		return matched
	}

	waitForCondition(t, 5*time.Second, "cron_lease_acquired and a fire", func() bool {
		return len(eventsOfKind(events.EventCronLeaseAcquired)) >= 1 &&
			len(eventsOfKind(events.EventPipelineCompleted)) >= 1
	})

	acquired := eventsOfKind(events.EventCronLeaseAcquired)[0]
	if acquired.Payload["lease"] != leaseName || acquired.Payload["holder"] != "replica-a" {
		t.Fatalf("cron_lease_acquired payload = %v, want lease %q holder replica-a", acquired.Payload, leaseName)
	}
	if _, ok := acquired.Payload["fence"]; !ok {
		t.Fatalf("cron_lease_acquired payload missing fence: %v", acquired.Payload)
	}
	if _, ok := acquired.Payload["expires_at"]; !ok {
		t.Fatalf("cron_lease_acquired payload missing expires_at: %v", acquired.Payload)
	}

	fired := eventsOfKind(events.EventPipelineCompleted)[0]
	if fired.Payload["lease"] != leaseName {
		t.Fatalf("cron fire payload lease = %v, want %q", fired.Payload["lease"], leaseName)
	}
	if fired.Payload["fence"] == nil {
		t.Fatalf("cron fire payload missing fence: %v", fired.Payload)
	}

	// Steal the lease (tombstone + takeover at fence+1); the leader's next
	// renewal must fail, emit cron_lease_lost, and drop it to follower.
	current, err := store.Leases(context.Background())
	if err != nil || len(current) != 1 {
		t.Fatalf("Leases = %v err=%v, want exactly one", current, err)
	}
	if err := store.ReleaseLease(context.Background(), leaseName, current[0].Holder, current[0].Fence); err != nil {
		t.Fatalf("ReleaseLease returned error: %v", err)
	}
	if _, acquiredByThief, err := store.AcquireLease(context.Background(), leaseName, "thief", time.Hour); err != nil || !acquiredByThief {
		t.Fatalf("thief AcquireLease acquired=%v err=%v", acquiredByThief, err)
	}

	waitForCondition(t, 5*time.Second, "cron_lease_lost after takeover", func() bool {
		return len(eventsOfKind(events.EventCronLeaseLost)) >= 1
	})
	lost := eventsOfKind(events.EventCronLeaseLost)[0]
	if lost.Payload["lease"] != leaseName || lost.Payload["holder"] != "replica-a" {
		t.Fatalf("cron_lease_lost payload = %v", lost.Payload)
	}

	cancel()
	<-done
}

func TestUnleasedCronLoopStillRunsWithLeaseStoreBackend(t *testing.T) {
	// OUVRIER_CRON_LEASE=off restores the v0.2 unleased loop even on a
	// lease-capable backend: no lease rows are ever written.
	t.Setenv("OUVRIER_CRON_LEASE", "off")
	store := state.NewMemoryStore()
	rt := httpRuntime{stateStore: store}

	plans, err := compilePlans([]Node{
		From(Cron("@every 100ms")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	schedules, err := cronSchedulesForPlans(plans)
	if err != nil {
		t.Fatalf("cronSchedulesForPlans returned error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runCronPlansWithSchedules(ctx, rt, plans, schedules)
	}()

	waitForCondition(t, 5*time.Second, "an unleased cron fire", func() bool {
		executions, err := store.Executions(context.Background())
		return err == nil && len(executions) >= 1
	})
	leases, err := store.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("leases written in unleased mode: %v", leases)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runCronPlansWithSchedules returned error: %v", err)
	}
}

func TestHTTPAdminHealthReportsCronLeases(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	t.Setenv("OUVRIER_REPLICA_ID", "health-self")
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	if _, acquired, err := store.AcquireLease(context.Background(), "cron:ab12cd34:0", "health-self", time.Minute); err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := store.AcquireLease(context.Background(), "cron:ff00ff00:1", "other-replica", time.Minute); err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := store.AcquireLease(context.Background(), "run:not-cron", "health-self", time.Minute); err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
	}

	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: store, eventStream: stream})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body struct {
		CronLeases []struct {
			Name      string `json:"name"`
			Holder    string `json:"holder"`
			Fence     uint64 `json:"fence"`
			ExpiresAt string `json:"expires_at"`
			IsSelf    bool   `json:"is_self"`
		} `json:"cron_leases"`
	}
	decodeAdminJSON(t, rec, &body)
	if len(body.CronLeases) != 2 {
		t.Fatalf("cron_leases = %+v, want exactly the two cron: leases", body.CronLeases)
	}
	self := body.CronLeases[0]
	if self.Name != "cron:ab12cd34:0" || self.Holder != "health-self" || self.Fence != 1 || !self.IsSelf {
		t.Fatalf("self lease = %+v", self)
	}
	if _, err := time.Parse(time.RFC3339Nano, self.ExpiresAt); err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", self.ExpiresAt, err)
	}
	other := body.CronLeases[1]
	if other.Name != "cron:ff00ff00:1" || other.Holder != "other-replica" || other.IsSelf {
		t.Fatalf("other lease = %+v", other)
	}
}

func TestHTTPAdminHealthOmitsCronLeasesWhenNoneHeld(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	handler := newTestAdminHTTPHandler(t, httpRuntime{stateStore: state.NewMemoryStore(), eventStream: stream})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	decodeAdminJSON(t, rec, &body)
	if _, ok := body["cron_leases"]; ok {
		t.Fatalf("cron_leases present without cron leases: %v", body["cron_leases"])
	}
}

func TestLeasedCronTwoRuntimesOnPostgresFireEachTickExactlyOnce(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OUVRIER_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("OUVRIER_TEST_POSTGRES_DSN not set; skipping postgres leased cron test")
	}
	t.Setenv("OUVRIER_ENV", "dev") // silence the sslmode=disable startup warning

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read returned error: %v", err)
	}
	schema := "ouvrier_cron_lease_" + hex.EncodeToString(raw[:])
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close()
	})
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	scopedDSN := dsn + separator + "search_path=" + schema

	openStore := func() *state.PostgresStore {
		store, err := state.NewPostgresStore(scopedDSN)
		if err != nil {
			t.Fatalf("NewPostgresStore returned error: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	storeA := openStore()
	storeB := openStore()

	recorder := &cronFireRecorder{}
	webhook := httptest.NewServer(recorder.handler())
	defer webhook.Close()
	plans, schedules := compileCronWebhookPlans(t, "@every 200ms", webhook.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfgA := cronLeaseConfig{leases: storeA, holder: "replica-a", ttl: 2 * time.Second, poll: 100 * time.Millisecond}
	cfgB := cronLeaseConfig{leases: storeB, holder: "replica-b", ttl: 2 * time.Second, poll: 100 * time.Millisecond}
	doneA := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, storeA), plans, schedules, cfgA)
	doneB := startLeasedCronReplica(ctx, newLeasedCronRuntime(t, storeB), plans, schedules, cfgB)

	waitForCondition(t, 15*time.Second, "at least four cron fires", func() bool {
		return recorder.count() >= 4
	})
	cancel()
	<-doneA
	<-doneB

	assertDistinctFires(t, recorder.snapshot())
}
