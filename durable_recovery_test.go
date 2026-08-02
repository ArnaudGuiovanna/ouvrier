package ovr

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// Durable-run recovery tests (#40). The "kill -9" simulation is a SECOND
// runtime over the same state store with the first runtime's journal rows
// present: rows are seeded exactly as the write side leaves them (running
// execution + journal + checkpoints + intents) and neither finish nor prune
// is ever called for the abandoned run.

func newDurableRecoveryTestConfig(scan time.Duration) *durableRunsConfig {
	cfg := newDurableRunsConfig(0)
	cfg.recovery = &durableRecoveryConfig{scan: scan, concurrency: 2}
	return cfg
}

func compileDurableTestPlan(t *testing.T, nodes []Node) runtimeplan.Plan {
	t.Helper()
	plans, err := compilePlans(nodes)
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("compilePlans = %d plans, want 1", len(plans))
	}
	return plans[0]
}

// seedInterruptedDurableRun leaves behind exactly what a killed runtime
// leaves: a running execution row plus the run journal row.
func seedInterruptedDurableRun(t *testing.T, store state.Store, plan runtimeplan.Plan, execID, traceID, input string) {
	t.Helper()
	if err := store.SaveExecution(context.Background(), state.Execution{
		ExecID:    execID,
		TraceID:   traceID,
		Status:    state.ExecutionRunning,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed SaveExecution returned error: %v", err)
	}
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID:      execID,
		PlanKey:     durablePlanKey(plan),
		PlanHash:    durablePlanHash(plan),
		TriggerKind: string(plan.Trigger.Kind),
		Input:       input,
	}); err != nil {
		t.Fatalf("seed SaveRunJournal returned error: %v", err)
	}
}

func seedDurableCheckpoint(t *testing.T, store state.Store, execID string, stepIndex int, output string) {
	t.Helper()
	if err := store.SaveRunCheckpoint(context.Background(), state.RunCheckpoint{
		ExecID:    execID,
		StepIndex: stepIndex,
		Output:    output,
	}); err != nil {
		t.Fatalf("seed SaveRunCheckpoint returned error: %v", err)
	}
}

func runDurableRecoveryScanNow(t *testing.T, rt httpRuntime, plans ...runtimeplan.Plan) {
	t.Helper()
	leases, ok := rt.stateStore.(state.LeaseStore)
	if !ok {
		t.Fatal("state store has no lease capability")
	}
	// syncEvents=true mirrors the loop's first scan: manual test scans always
	// re-sync the event stream so seeded store events keep IDs monotonic.
	rt.recoverDurableRunsScan(context.Background(), leases, plans, newCronWorkerPool(2), true)
}

func newDurableRecoveryEventStream(t *testing.T) *events.EventStream {
	t.Helper()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	return stream
}

func recoveryEventsOfKind(t *testing.T, store state.Store, execID string, kind events.EventKind) []events.Event {
	t.Helper()
	recorded, err := store.Events(context.Background(), execID)
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	matched := []events.Event{}
	for _, event := range recorded {
		if event.Kind == kind {
			matched = append(matched, event)
		}
	}
	return matched
}

func durableExecutionStatus(t *testing.T, store state.Store, execID string) state.ExecutionStatus {
	t.Helper()
	execution, ok, err := store.Execution(context.Background(), execID)
	if err != nil || !ok {
		t.Fatalf("Execution(%s) ok=%v err=%v", execID, ok, err)
	}
	return execution.Status
}

// TestDurableRecoveryReplaysReadOnlyRunFromLastCheckpoint is the
// kill-between-steps acceptance test: a run checkpointed past its
// side-effecting step is resumed by a second runtime from that checkpoint and
// completes without the side-effecting tool ever running again.
func TestDurableRecoveryReplaysReadOnlyRunFromLastCheckpoint(t *testing.T) {
	store := newDurableTestStore(t)
	sideEffects := 0
	nodes := []Node{
		From("POST /tickets"),
		Pipe("step zero",
			Model("durable/r0"),
			Tool("send_email", func(ctx context.Context) error { sideEffects++; return nil }, SideEffecting("email")),
		),
		Pipe("step one", Model("durable/r1")),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)

	const execID = "exec_recover_readonly"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_recover_readonly", `{"title":"broken"}`)
	seedDurableCheckpoint(t, store, execID, 0, `{"step":"zero"}`)
	// The side-effecting call of step 0 completed before the kill; its intent
	// sits on a checkpointed step and must not block the replay.
	if err := store.BeginToolIntent(context.Background(), state.ToolIntent{
		ExecID: execID, ToolCallID: "call_email_1", StepIndex: 0,
		ToolName: "send_email", Effect: string(policy.EffectSideEffecting), IdemKey: "args:abc",
	}); err != nil {
		t.Fatalf("seed BeginToolIntent returned error: %v", err)
	}
	if err := store.CompleteToolIntent(context.Background(), execID, "call_email_1"); err != nil {
		t.Fatalf("seed CompleteToolIntent returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/r1":
			return endTurn(`{"status":"done"}`)
		default:
			t.Errorf("recovered run called model %q, want only the unfinished step durable/r1", model)
			return provider.Response{}, nil
		}
	}}
	rt := httpRuntime{
		provider:   scripted,
		stateStore: store,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffects("email")),
		)),
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}

	runDurableRecoveryScanNow(t, rt, plan)

	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionCompleted {
		t.Fatalf("execution status = %q, want completed after replay", status)
	}
	if sideEffects != 0 {
		t.Fatalf("side-effecting tool ran %d times during replay, want 0 (step 0 was checkpointed)", sideEffects)
	}
	journals, err := store.RunJournals(context.Background())
	if err != nil || len(journals) != 0 {
		t.Fatalf("journals after replay = %v, %v, want pruned on success", journals, err)
	}
	recovered := recoveryEventsOfKind(t, store, execID, events.EventRunRecovered)
	if len(recovered) != 1 {
		t.Fatalf("run_recovered events = %+v, want exactly one", recovered)
	}
	if step, _ := recovered[0].Payload["resumed_from_step"].(float64); int(step) != 1 {
		t.Fatalf("run_recovered resumed_from_step = %v, want 1", recovered[0].Payload["resumed_from_step"])
	}
}

func TestDurableRecoveryRefusesRedactedReplayInputFailClosed(t *testing.T) {
	store := newDurableTestStore(t)
	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("classify", Model("durable/redacted")),
		Reply(Accepted()),
	})

	const execID = "exec_recover_redacted"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_recover_redacted",
		`{"ticket":"safe","password":"must-not-replay"}`)
	journal, ok, err := store.RunJournal(context.Background(), execID)
	if err != nil || !ok || !journal.ReplayUnsafe {
		t.Fatalf("seeded journal = %+v ok=%v err=%v, want replay-unsafe", journal, ok, err)
	}

	providerCalls := 0
	rt := httpRuntime{
		provider: &durableTestProvider{complete: func(string, int) (provider.Response, error) {
			providerCalls++
			return endTurn(`{"unexpected":true}`)
		}},
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rt, plan)

	if providerCalls != 0 {
		t.Fatalf("provider calls = %d, want zero for unsafe replay input", providerCalls)
	}
	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("unsafe journal retained ok=%v err=%v, want retained for inspection", ok, err)
	}
	events := recoveryEventsOfKind(t, store, execID, events.EventRunAbandoned)
	if len(events) != 1 || events[0].Payload["reason"] != "replay_input_redacted" || events[0].Payload["source"] != "journal_input" {
		t.Fatalf("run_abandoned events = %+v, want explicit redacted-input refusal", events)
	}
}

// TestDurableRecoveryFailsLoudOnOpenWriteIntentAndOperatorRecovers is the
// kill-mid-write acceptance test: an open side-effecting intent at the
// interrupted step is never auto-replayed — the run fails with
// replay_indeterminate_tool, shows under /admin/runs?status=orphaned, and an
// operator-forced POST .../recover replays it to completion.
func TestDurableRecoveryFailsLoudOnOpenWriteIntentAndOperatorRecovers(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := []Node{
		From("POST /tickets"),
		Pipe("step zero", Model("durable/w0")),
		Pipe("write step",
			Model("durable/w1"),
			Tool("send_email", func(ctx context.Context) error { return nil }, SideEffecting("email")),
		),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)

	const execID = "exec_recover_indeterminate"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_recover_indeterminate", `{"title":"broken"}`)
	seedDurableCheckpoint(t, store, execID, 0, `{"step":"zero"}`)
	// Killed mid write-tool: the intent is open (no CompleteToolIntent).
	if err := store.BeginToolIntent(context.Background(), state.ToolIntent{
		ExecID: execID, ToolCallID: "call_email_1", StepIndex: 1,
		ToolName: "send_email", Effect: string(policy.EffectSideEffecting), IdemKey: "args:abc",
	}); err != nil {
		t.Fatalf("seed BeginToolIntent returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/w1":
			return endTurn(`{"status":"done"}`)
		default:
			t.Errorf("forced replay called model %q, want only durable/w1", model)
			return provider.Response{}, nil
		}
	}}
	rt := httpRuntime{
		provider:   scripted,
		stateStore: store,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffects("email")),
		)),
		eventStream: newDurableRecoveryEventStream(t),
		adminToken:  "secret",
		// recovery stays nil: this test drives scans explicitly, so the
		// handler below must not start a competing background loop.
		durableRuns: newDurableRunsConfig(0),
	}

	runDurableRecoveryScanNow(t, rt, plan)

	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed-loud on open write intent", status)
	}
	indeterminate := recoveryEventsOfKind(t, store, execID, events.EventReplayIndeterminateTool)
	if len(indeterminate) != 1 {
		t.Fatalf("replay_indeterminate_tool events = %+v, want exactly one", indeterminate)
	}
	if tool, _ := indeterminate[0].Payload["tool"].(string); tool != "send_email" {
		t.Fatalf("indeterminate payload tool = %v, want send_email", indeterminate[0].Payload)
	}
	if open, _ := indeterminate[0].Payload["open"].(bool); !open {
		t.Fatalf("indeterminate payload open = %v, want true", indeterminate[0].Payload)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("journal after indeterminate marking ok=%v err=%v, want kept for the operator", ok, err)
	}

	handler, err := newHTTPHandlerWithRuntime(nodes, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	listRec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/admin/runs?status=orphaned", nil)
	listReq.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /admin/runs status = %d body=%s, want %d", listRec.Code, listRec.Body.String(), http.StatusOK)
	}
	var list adminRunsResponse
	decodeAdminJSON(t, listRec, &list)
	if len(list.Runs) != 1 || list.Runs[0].ExecID != execID {
		t.Fatalf("orphaned runs = %+v, want exactly %s", list.Runs, execID)
	}
	run := list.Runs[0]
	if !run.Orphaned || run.ExecutionStatus != string(state.ExecutionFailed) || run.OpenIntents != 1 || run.Checkpoints != 1 {
		t.Fatalf("orphaned run = %+v, want orphaned failed run with one open intent and one checkpoint", run)
	}

	recoverRec := httptest.NewRecorder()
	recoverReq := httptest.NewRequest(http.MethodPost, "/admin/runs/"+execID+"/recover", nil)
	recoverReq.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(recoverRec, recoverReq)
	if recoverRec.Code != http.StatusAccepted {
		t.Fatalf("POST recover status = %d body=%s, want %d", recoverRec.Code, recoverRec.Body.String(), http.StatusAccepted)
	}

	// finishPipelineExecution prunes the journal BEFORE saving the completed
	// execution, so poll on the status (the last write of the replay) and only
	// then assert the prune — polling on the prune races the final
	// SaveExecution.
	waitForCondition(t, 5*time.Second, "forced replay completes the execution", func() bool {
		execution, ok, err := store.Execution(context.Background(), execID)
		return err == nil && ok && execution.Status == state.ExecutionCompleted
	})
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || ok {
		t.Fatalf("journal ok=%v err=%v, want pruned after forced replay", ok, err)
	}
}

// TestDurableRecoveryFailsLoudOnCompletedSideEffectingIntent covers the other
// half of the fail-loud policy: a COMPLETED side-effecting intent on the
// interrupted step means the effect already happened and has no dedup, so an
// auto-replay would duplicate it — the scan must mark the run
// replay_indeterminate_tool, exactly like the open-intent case.
func TestDurableRecoveryFailsLoudOnCompletedSideEffectingIntent(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := []Node{
		From("POST /tickets"),
		Pipe("step zero", Model("durable/c0")),
		Pipe("write step",
			Model("durable/c1"),
			Tool("send_email", func(ctx context.Context) error { return nil }, SideEffecting("email")),
		),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)

	const execID = "exec_recover_completed_intent"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_recover_completed_intent", `{"title":"broken"}`)
	seedDurableCheckpoint(t, store, execID, 0, `{"step":"zero"}`)
	// Killed AFTER the write tool returned but BEFORE the step checkpointed:
	// the intent is completed, but replaying the step would send the email
	// twice.
	if err := store.BeginToolIntent(context.Background(), state.ToolIntent{
		ExecID: execID, ToolCallID: "call_email_1", StepIndex: 1,
		ToolName: "send_email", Effect: string(policy.EffectSideEffecting), IdemKey: "args:abc",
	}); err != nil {
		t.Fatalf("seed BeginToolIntent returned error: %v", err)
	}
	if err := store.CompleteToolIntent(context.Background(), execID, "call_email_1"); err != nil {
		t.Fatalf("seed CompleteToolIntent returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		t.Errorf("scan called model %q, want no auto-replay over a completed side-effecting intent", model)
		return endTurn(`{"status":"duplicated"}`)
	}}
	rt := httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rt, plan)

	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed-loud on completed side-effecting intent", status)
	}
	indeterminate := recoveryEventsOfKind(t, store, execID, events.EventReplayIndeterminateTool)
	if len(indeterminate) != 1 {
		t.Fatalf("replay_indeterminate_tool events = %+v, want exactly one", indeterminate)
	}
	if tool, _ := indeterminate[0].Payload["tool"].(string); tool != "send_email" {
		t.Fatalf("indeterminate payload tool = %v, want send_email", indeterminate[0].Payload)
	}
	if open, ok := indeterminate[0].Payload["open"].(bool); !ok || open {
		t.Fatalf("indeterminate payload open = %v, want false (the intent completed)", indeterminate[0].Payload)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("journal after indeterminate marking ok=%v err=%v, want kept for the operator", ok, err)
	}
}

// TestDurableRecoveryNeverStealsHeartbeatedLease is the two-store
// lease-protection acceptance test: a slow-but-alive run holding its
// heartbeated lease on store A is never claimed by store B's recovery scan.
func TestDurableRecoveryNeverStealsHeartbeatedLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	openStore := func() *state.SQLiteStore {
		store, err := state.NewSQLiteStore(path)
		if err != nil {
			t.Fatalf("NewSQLiteStore returned error: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	storeA := openStore()
	storeB := openStore()
	assertDurableRecoverySkipsLiveLease(t, storeA, storeB)
}

// TestDurableRecoveryNeverStealsHeartbeatedLeaseOnPostgres is the DSN-gated
// Postgres variant of the lease-protection test.
func TestDurableRecoveryNeverStealsHeartbeatedLeaseOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OUVRIER_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("OUVRIER_TEST_POSTGRES_DSN not set; skipping postgres run-lease test")
	}
	t.Setenv("OUVRIER_ENV", "dev") // silence the sslmode=disable startup warning

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand.Read returned error: %v", err)
	}
	schema := "ouvrier_run_lease_" + hex.EncodeToString(raw[:])
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
	assertDurableRecoverySkipsLiveLease(t, openStore(), openStore())
}

// assertDurableRecoverySkipsLiveLease drives the shared two-store scenario:
// replica A heartbeats run:<exec>; replica B scans and must not touch the run.
func assertDurableRecoverySkipsLiveLease(t *testing.T, storeA, storeB state.Store) {
	t.Helper()
	nodes := []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/live")),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)
	const execID = "exec_live_run"
	seedInterruptedDurableRun(t, storeA, plan, execID, "trace_live_run", `{"title":"slow"}`)

	leasesA, ok := storeA.(state.LeaseStore)
	if !ok {
		t.Fatal("store A has no lease capability")
	}
	lease, acquired, err := leasesA.AcquireLease(context.Background(), durableRunLeaseName(execID), "replica-a", 30*time.Second)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v, want replica A holding the run lease", acquired, err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		t.Errorf("replica B called model %q while replica A holds the run lease", model)
		return endTurn(`{"status":"stolen"}`)
	}}
	rtB := httpRuntime{
		provider:    scripted,
		stateStore:  storeB,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rtB, plan)

	if status := durableExecutionStatus(t, storeB, execID); status != state.ExecutionRunning {
		t.Fatalf("execution status = %q, want still running (never stolen)", status)
	}
	if _, ok, err := storeB.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("journal ok=%v err=%v, want untouched", ok, err)
	}
	leasesB := storeB.(state.LeaseStore)
	rows, err := leasesB.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	for _, row := range rows {
		if row.Name != durableRunLeaseName(execID) {
			continue
		}
		if row.Holder != "replica-a" || row.Fence != lease.Fence {
			t.Fatalf("run lease = %+v, want replica-a's lease untouched at fence %d", row, lease.Fence)
		}
	}
}

// TestDurableRecoveryAbandonsEditedPlan is the plan_hash-mismatch acceptance
// test: an edited pipeline yields a clean run_abandoned, never a mixed replay.
func TestDurableRecoveryAbandonsEditedPlan(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/edited")),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)

	const execID = "exec_edited_plan"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_edited_plan", `{"title":"broken"}`)
	// Simulate a redeploy that edited the pipeline: the journal's hash no
	// longer matches the current compiled steps.
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID:      execID,
		PlanKey:     durablePlanKey(plan),
		PlanHash:    "hash-of-the-previous-deploy",
		TriggerKind: string(plan.Trigger.Kind),
		Input:       `{"title":"broken"}`,
	}); err != nil {
		t.Fatalf("SaveRunJournal returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		t.Errorf("abandoned run called model %q, want no replay on plan_hash mismatch", model)
		return endTurn(`{"status":"mixed"}`)
	}}
	rt := httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rt, plan)

	abandoned := recoveryEventsOfKind(t, store, execID, events.EventRunAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("run_abandoned events = %+v, want exactly one", abandoned)
	}
	if reason, _ := abandoned[0].Payload["reason"].(string); reason != "plan_hash_mismatch" {
		t.Fatalf("run_abandoned reason = %v, want plan_hash_mismatch", abandoned[0].Payload)
	}
	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || ok {
		t.Fatalf("journal ok=%v err=%v, want pruned on abandonment", ok, err)
	}
}

// TestDurableRecoveryAbandonsChangedTerminal proves that the recovery hash
// binds the observable destination, not only the pipeline steps. Replaying an
// old journal into a newly configured webhook would otherwise redirect a
// side effect after a deployment.
func TestDurableRecoveryAbandonsChangedTerminal(t *testing.T) {
	store := newDurableTestStore(t)
	original := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/terminal-change")),
		Push(Webhook("https://old.example.invalid/results")),
	})
	current := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/terminal-change")),
		Push(Webhook("https://new.example.invalid/results")),
	})

	const execID = "exec_changed_terminal"
	seedInterruptedDurableRun(t, store, original, execID, "trace_changed_terminal", `{"title":"broken"}`)

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		t.Errorf("terminal-mismatched run called model %q, want fail-closed abandonment", model)
		return endTurn(`{"status":"misdirected"}`)
	}}
	rt := httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rt, current)

	abandoned := recoveryEventsOfKind(t, store, execID, events.EventRunAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("run_abandoned events = %+v, want exactly one", abandoned)
	}
	if reason, _ := abandoned[0].Payload["reason"].(string); reason != "plan_hash_mismatch" {
		t.Fatalf("run_abandoned reason = %v, want plan_hash_mismatch", abandoned[0].Payload)
	}
	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || ok {
		t.Fatalf("journal ok=%v err=%v, want pruned on abandonment", ok, err)
	}
}

// TestDurableRecoveryAbandonsJournalFromDifferentWorkerBuild proves that two
// binaries compiling the same plan cannot mix handler or tool code during a
// replay. The combined v3 hash needs no journal-schema migration: journals
// written by v2 or by another executable fail the existing mismatch path.
func TestDurableRecoveryAbandonsJournalFromDifferentWorkerBuild(t *testing.T) {
	store := newDurableTestStore(t)
	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("unchanged plan", Model("durable/build-change")),
		Reply(Accepted()),
	})

	const execID = "exec_changed_worker_build"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_changed_worker_build", `{"title":"broken"}`)
	previousBuildHash := durablePlanHashWithBuildIdentity(plan, "sha256:previous-worker-build")
	if previousBuildHash == durablePlanHash(plan) {
		t.Fatal("test previous-build identity unexpectedly matches the running executable")
	}
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID:      execID,
		PlanKey:     durablePlanKey(plan),
		PlanHash:    previousBuildHash,
		TriggerKind: string(plan.Trigger.Kind),
		Input:       `{"title":"broken"}`,
	}); err != nil {
		t.Fatalf("SaveRunJournal returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		t.Errorf("build-mismatched run called model %q, want fail-closed abandonment", model)
		return endTurn(`{"status":"mixed-code"}`)
	}}
	rt := httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rt, plan)

	abandoned := recoveryEventsOfKind(t, store, execID, events.EventRunAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("run_abandoned events = %+v, want exactly one", abandoned)
	}
	if reason, _ := abandoned[0].Payload["reason"].(string); reason != "plan_hash_mismatch" {
		t.Fatalf("run_abandoned reason = %v, want plan_hash_mismatch", abandoned[0].Payload)
	}
	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || ok {
		t.Fatalf("journal ok=%v err=%v, want pruned on abandonment", ok, err)
	}
}

// TestDurableRecoveryAbandonsSyncReplyRun: the suspended caller of a
// synchronous Reply vanished with the crashed process, so recovery abandons
// instead of replaying into nowhere.
func TestDurableRecoveryAbandonsSyncReplyRun(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/sync")),
		Reply(JSON[httpTestReply]()),
	}
	plan := compileDurableTestPlan(t, nodes)

	const execID = "exec_sync_reply"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_sync_reply", `{"title":"broken"}`)

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		t.Errorf("sync-reply run called model %q, want abandonment (client gone)", model)
		return endTurn(`{"status":"ghost"}`)
	}}
	rt := httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(time.Minute),
	}
	runDurableRecoveryScanNow(t, rt, plan)

	abandoned := recoveryEventsOfKind(t, store, execID, events.EventRunAbandoned)
	if len(abandoned) != 1 {
		t.Fatalf("run_abandoned events = %+v, want exactly one", abandoned)
	}
	if reason, _ := abandoned[0].Payload["reason"].(string); reason != "client_gone" {
		t.Fatalf("run_abandoned reason = %v, want client_gone", abandoned[0].Payload)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || ok {
		t.Fatalf("journal ok=%v err=%v, want pruned on abandonment", ok, err)
	}
}

// TestDurableRecoveryApprovalFallbackResumesAcrossRestart exercises the
// cross-restart approval path: runtime A suspends on a gated tool and dies;
// runtime B approves the record (in-memory registry empty => resumed=false)
// and its recovery scan replays the suspended step, auto-allowing the gated
// call against the approved record's args_hash.
func TestDurableRecoveryApprovalFallbackResumesAcrossRestart(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := func(wirePayments *int) []Node {
		return []Node{
			From("POST /tickets"),
			Pipe("step zero", Model("durable/a0")),
			Pipe("gated step",
				Model("durable/agated"),
				Tool("wire_payment", func(ctx context.Context) error {
					if wirePayments != nil {
						*wirePayments += 1
					}
					return nil
				}, RequiresApproval()),
			),
			Reply(Accepted()),
		}
	}

	wireCall := provider.ToolCall{ID: "call_wire_1", Name: "wire_payment"}
	scriptedA := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/a0":
			return endTurn(`{"step":"zero"}`)
		case "durable/agated":
			return provider.Response{Text: "need approval", StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{wireCall}}, nil
		default:
			t.Errorf("runtime A called unexpected model %q", model)
			return provider.Response{}, nil
		}
	}}
	rtA := httpRuntime{
		provider:    scriptedA,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		adminToken:  "secret",
		// recovery stays nil on both runtimes: this test drives scans
		// explicitly, so the handlers must not start competing loops.
		durableRuns: newDurableRunsConfig(0),
	}
	handlerA, err := newHTTPHandlerWithRuntime(nodes(nil), rtA)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime(A) returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handlerA.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"wire"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s, want async 202", rec.Code, rec.Body.String())
	}
	var approval state.PendingApproval
	waitForCondition(t, 5*time.Second, "suspension records a pending approval", func() bool {
		pending, err := store.PendingApprovals(context.Background())
		if err != nil || len(pending) != 1 {
			return false
		}
		approval = pending[0]
		return true
	})
	if approval.ArgsHash == "" {
		t.Fatalf("approval = %+v, want args_hash recorded at suspend time", approval)
	}
	execID := approval.ExecID
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("journal ok=%v err=%v, want kept while suspended", ok, err)
	}
	// The suspended run's deferred lease release runs just after the approval
	// record appears; wait it out so the recovery scans below observe the
	// parked (lease-free) state deterministically.
	waitForCondition(t, 5*time.Second, "suspended run releases its run lease", func() bool {
		rows, err := store.Leases(context.Background())
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.Name == durableRunLeaseName(execID) && row.ExpiresAt.After(time.Now()) {
				return false
			}
		}
		return true
	})

	// "Kill -9" runtime A: drop it without finish/prune. Runtime B comes up
	// over the same database with an empty in-memory resume registry.
	wirePayments := 0
	scriptedB := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/agated":
			if call == 0 {
				return provider.Response{Text: "retrying gated call", StopReason: provider.StopToolUse,
					ToolCalls: []provider.ToolCall{{ID: "call_wire_replayed", Name: "wire_payment"}}}, nil
			}
			return endTurn(`{"step":"gated"}`)
		default:
			t.Errorf("runtime B called model %q, want only the suspended step durable/agated", model)
			return provider.Response{}, nil
		}
	}}
	rtB := httpRuntime{
		provider:    scriptedB,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		adminToken:  "secret",
		durableRuns: newDurableRunsConfig(0),
	}
	if err := rtB.syncEventStreamWithStore(context.Background()); err != nil {
		t.Fatalf("syncEventStreamWithStore returned error: %v", err)
	}
	planB := compileDurableTestPlan(t, nodes(&wirePayments))
	handlerB, err := newHTTPHandlerWithRuntime(nodes(&wirePayments), rtB)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime(B) returned error: %v", err)
	}

	// While the approval is pending the run is parked, not orphaned: a scan
	// must not claim it (replaying would mint a duplicate approval).
	runDurableRecoveryScanNow(t, rtB, planB)
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("journal ok=%v err=%v, want untouched while approval is pending", ok, err)
	}
	approvals, err := store.ApprovalsForExecution(context.Background(), execID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals after parked scan = %v, %v, want the single pending record", approvals, err)
	}

	decisionRec := httptest.NewRecorder()
	decisionReq := httptest.NewRequest(http.MethodPost, "/admin/approvals/"+approval.ID,
		strings.NewReader(`{"decision":"approve","decided_by":"ops"}`))
	decisionReq.Header.Set("Authorization", "Bearer secret")
	handlerB.ServeHTTP(decisionRec, decisionReq)
	if decisionRec.Code != http.StatusOK {
		t.Fatalf("approval decision status = %d body=%s, want %d", decisionRec.Code, decisionRec.Body.String(), http.StatusOK)
	}
	var decision adminApprovalDecisionResponse
	decodeAdminJSON(t, decisionRec, &decision)
	if decision.Resumed {
		t.Fatal("decision resumed = true, want false: runtime B has no in-memory resume for runtime A's suspension")
	}

	runDurableRecoveryScanNow(t, rtB, planB)

	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionCompleted {
		t.Fatalf("execution status = %q, want completed via approval fallback replay", status)
	}
	if wirePayments != 1 {
		t.Fatalf("wire_payment executed %d times, want exactly once on the auto-allowed replay", wirePayments)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || ok {
		t.Fatalf("journal ok=%v err=%v, want pruned after completed replay", ok, err)
	}
	approvals, err = store.ApprovalsForExecution(context.Background(), execID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("approvals after replay = %v, %v, want no duplicate approval minted", approvals, err)
	}
}

// TestDurableRunsHeartbeatHoldsRunLeaseDuringExecution: while a journaled run
// executes, its run:<exec_id> lease is live; once it finishes, the lease is
// tombstoned so the journal row never looks live after the fact.
func TestDurableRunsHeartbeatHoldsRunLeaseDuringExecution(t *testing.T) {
	store := newDurableTestStore(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return endTurn(`{"status":"done"}`)
	}}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("slow step", Model("durable/slow")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := postDurableTicket(t, handler)
		done <- rec
	}()
	<-started

	leases := store
	rows, err := leases.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	var runLease state.Lease
	found := false
	for _, row := range rows {
		if strings.HasPrefix(row.Name, "run:") {
			runLease = row
			found = true
		}
	}
	if !found {
		t.Fatalf("leases = %+v, want a run:<exec_id> lease while the durable run executes", rows)
	}
	if !runLease.ExpiresAt.After(time.Now()) {
		t.Fatalf("run lease %+v expired mid-run, want live heartbeat", runLease)
	}

	close(release)
	rec := <-done
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusOK)
	}

	rows, err = leases.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	for _, row := range rows {
		if row.Name == runLease.Name && row.ExpiresAt.After(time.Now()) {
			t.Fatalf("run lease %+v still live after the run finished, want released", row)
		}
	}
}

// TestDurableRecoveryLoopRecoversWithoutManualScan wires the periodic loop
// end to end: a handler built with a short scan interval abandons a seeded
// edited-plan journal on its own.
func TestDurableRecoveryLoopRecoversWithoutManualScan(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/loop")),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)
	const execID = "exec_loop_recovered"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_loop_recovered", `{"title":"broken"}`)
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID:      execID,
		PlanKey:     durablePlanKey(plan),
		PlanHash:    "hash-of-the-previous-deploy",
		TriggerKind: string(plan.Trigger.Kind),
		Input:       `{"title":"broken"}`,
	}); err != nil {
		t.Fatalf("SaveRunJournal returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		return endTurn(`{"status":"done"}`)
	}}
	handler, err := newHTTPHandlerWithRuntime(nodes, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		durableRuns: newDurableRecoveryTestConfig(50 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	t.Cleanup(func() {
		if shutdownable, ok := handler.(interface{ Shutdown(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownable.Shutdown(ctx)
		}
	})

	waitForCondition(t, 5*time.Second, "recovery loop abandons the edited-plan journal", func() bool {
		_, ok, err := store.RunJournal(context.Background(), execID)
		return err == nil && !ok
	})
	abandoned := recoveryEventsOfKind(t, store, execID, events.EventRunAbandoned)
	if len(abandoned) == 0 {
		t.Fatal("no run_abandoned event recorded by the periodic loop")
	}
}

// TestAdminRunsEndpointsRequireAuthAndValidateInput covers the admin surface
// edges: bearer auth, the status filter contract, and recover on unknown ids.
func TestAdminRunsEndpointsRequireAuthAndValidateInput(t *testing.T) {
	store := newDurableTestStore(t)
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/admin")),
		Reply(Accepted()),
	}, httpRuntime{
		provider:    &durableTestProvider{complete: func(string, int) (provider.Response, error) { return endTurn(`{}`) }},
		stateStore:  store,
		adminToken:  "secret",
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/runs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /admin/runs status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/runs?status=bogus", nil)
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /admin/runs?status=bogus status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/runs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/runs status = %d, want %d", rec.Code, http.StatusOK)
	}
	var list adminRunsResponse
	decodeAdminJSON(t, rec, &list)
	if list.Status != "ok" || len(list.Runs) != 0 {
		t.Fatalf("empty journal list = %+v, want ok with zero runs", list)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/runs/exec_absent/recover", nil)
	req.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("recover unknown exec status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/runs/exec_absent/recover", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated recover status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestAdminRunsRecoverRefusesActiveParkedAndCompletedRuns covers the recover
// endpoint's refusals: 409 run_active while a live run lease protects the
// run, 409 approval_pending for a run parked on a pending approval (a forced
// replay would mint a duplicate), and 409 run_completed for a completed
// execution whose journal row survived a prune failure.
func TestAdminRunsRecoverRefusesActiveParkedAndCompletedRuns(t *testing.T) {
	store := newDurableTestStore(t)
	nodes := []Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/refuse")),
		Reply(Accepted()),
	}
	plan := compileDurableTestPlan(t, nodes)
	handler, err := newHTTPHandlerWithRuntime(nodes, httpRuntime{
		provider: &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
			t.Errorf("refused recover called model %q, want no replay", model)
			return endTurn(`{"status":"replayed"}`)
		}},
		stateStore:  store,
		eventStream: newDurableRecoveryEventStream(t),
		adminToken:  "secret",
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}
	postRecover := func(t *testing.T, execID string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/runs/"+execID+"/recover", nil)
		req.Header.Set("Authorization", "Bearer secret")
		handler.ServeHTTP(rec, req)
		return rec
	}
	assertConflict := func(t *testing.T, rec *httptest.ResponseRecorder, code string) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("recover status = %d body=%s, want %d", rec.Code, rec.Body.String(), http.StatusConflict)
		}
		var response httpStatusResponse
		decodeAdminJSON(t, rec, &response)
		if response.Status != code {
			t.Fatalf("recover refusal code = %q, want %q", response.Status, code)
		}
	}

	// A live lease (the run is still heartbeating on another replica, or an
	// automatic recovery is in flight) wins over the operator.
	const liveExecID = "exec_recover_live"
	seedInterruptedDurableRun(t, store, plan, liveExecID, "trace_recover_live", `{"title":"live"}`)
	if _, acquired, err := store.AcquireLease(context.Background(), durableRunLeaseName(liveExecID), "replica-live", 30*time.Second); err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v, want the live run holding its lease", acquired, err)
	}
	assertConflict(t, postRecover(t, liveExecID), "run_active")
	if status := durableExecutionStatus(t, store, liveExecID); status != state.ExecutionRunning {
		t.Fatalf("live run status = %q, want untouched running", status)
	}

	// A run parked on a pending approval is suspended, not orphaned: forcing
	// a replay would mint a duplicate approval.
	const parkedExecID = "exec_recover_parked"
	seedInterruptedDurableRun(t, store, plan, parkedExecID, "trace_recover_parked", `{"title":"parked"}`)
	if err := store.SaveApproval(context.Background(), state.PendingApproval{
		ID: "appr_recover_parked", ExecID: parkedExecID, ToolName: "wire_payment",
		ToolCallID: "call_wire_1", Status: state.ApprovalPending,
	}); err != nil {
		t.Fatalf("SaveApproval returned error: %v", err)
	}
	assertConflict(t, postRecover(t, parkedExecID), "approval_pending")
	if status := durableExecutionStatus(t, store, parkedExecID); status != state.ExecutionRunning {
		t.Fatalf("parked run status = %q, want untouched running", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), parkedExecID); err != nil || !ok {
		t.Fatalf("parked journal ok=%v err=%v, want kept", ok, err)
	}

	// Redaction made the only replay input semantically incomplete. Even an
	// operator-forced replay cannot reconstruct it and is refused explicitly.
	const redactedExecID = "exec_recover_redacted_refused"
	seedInterruptedDurableRun(t, store, plan, redactedExecID, "trace_recover_redacted_refused",
		`{"ticket":"safe","password":"must-not-replay"}`)
	assertConflict(t, postRecover(t, redactedExecID), "replay_input_redacted")
	if status := durableExecutionStatus(t, store, redactedExecID); status != state.ExecutionRunning {
		t.Fatalf("redacted run status = %q, want untouched until recovery scan marks it failed", status)
	}

	// A journal row that survived a prune failure must never flip its
	// completed run back to running.
	const completedExecID = "exec_recover_completed"
	seedInterruptedDurableRun(t, store, plan, completedExecID, "trace_recover_completed", `{"title":"done"}`)
	if err := store.SaveExecution(context.Background(), state.Execution{
		ExecID:      completedExecID,
		TraceID:     "trace_recover_completed",
		Status:      state.ExecutionCompleted,
		StartedAt:   time.Now().UTC().Add(-time.Minute),
		CompletedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
	assertConflict(t, postRecover(t, completedExecID), "run_completed")
	if status := durableExecutionStatus(t, store, completedExecID); status != state.ExecutionCompleted {
		t.Fatalf("completed run status = %q, want still completed", status)
	}
}
