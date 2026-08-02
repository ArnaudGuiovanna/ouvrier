package ovr

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

type runLeaseRenewFailure int

const (
	runLeaseRenewTakeover runLeaseRenewFailure = iota
	runLeaseRenewStoreError
)

// controlledRunLeaseStore blocks the first run-lease renewal until the test
// releases it, then either transfers ownership to a new fence or returns a
// store error. Non-run leases keep the backend's regular behavior so these
// tests cannot alter the cron lease contract.
type controlledRunLeaseStore struct {
	*state.SQLiteStore
	mode          runLeaseRenewFailure
	renewEntered  chan struct{}
	releaseRenew  chan struct{}
	enteredOnce   sync.Once
	releaseOnce   sync.Once
	renewFailures atomic.Int32
}

func newControlledRunLeaseStore(t *testing.T, mode runLeaseRenewFailure) *controlledRunLeaseStore {
	t.Helper()
	return &controlledRunLeaseStore{
		SQLiteStore:  newDurableTestStore(t),
		mode:         mode,
		renewEntered: make(chan struct{}),
		releaseRenew: make(chan struct{}),
	}
}

func (s *controlledRunLeaseStore) RenewLease(ctx context.Context, name, holder string, fence uint64, ttl time.Duration) (state.Lease, bool, error) {
	if !strings.HasPrefix(name, "run:") || s.renewFailures.Load() > 0 {
		return s.SQLiteStore.RenewLease(ctx, name, holder, fence, ttl)
	}
	s.enteredOnce.Do(func() { close(s.renewEntered) })
	select {
	case <-ctx.Done():
		return state.Lease{}, false, ctx.Err()
	case <-s.releaseRenew:
	}
	s.renewFailures.Add(1)
	if s.mode == runLeaseRenewStoreError {
		return state.Lease{}, false, errors.New("injected lease store failure")
	}
	if err := s.SQLiteStore.ReleaseLease(ctx, name, holder, fence); err != nil {
		return state.Lease{}, false, err
	}
	taken, acquired, err := s.SQLiteStore.AcquireLease(ctx, name, "lease-thief", ttl)
	if err != nil {
		return state.Lease{}, false, err
	}
	if !acquired {
		return taken, false, errors.New("injected lease takeover did not acquire")
	}
	return taken, false, nil
}

func (s *controlledRunLeaseStore) loseRunLease(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.renewEntered:
	}
	s.releaseOnce.Do(func() { close(s.releaseRenew) })
	return nil
}

type leaseContextProvider struct {
	calls    atomic.Int32
	complete func(context.Context, provider.Request, int) (provider.Response, error)
}

func (p *leaseContextProvider) Name() string { return "lease-context-test" }

func (p *leaseContextProvider) Complete(ctx context.Context, request provider.Request) (provider.Response, error) {
	call := int(p.calls.Add(1) - 1)
	return p.complete(ctx, request, call)
}

func TestDurableRunLeaseTakeoverCancelsToolAndSkipsTerminal(t *testing.T) {
	store := newControlledRunLeaseStore(t, runLeaseRenewTakeover)
	cfg := newDurableRunsConfig(0)
	cfg.leaseTTL = 60 * time.Millisecond
	toolCancelled := atomic.Bool{}
	terminalCalls := atomic.Int32{}

	scripted := &leaseContextProvider{complete: func(_ context.Context, _ provider.Request, call int) (provider.Response, error) {
		if call == 0 {
			return provider.Response{
				Text:       "need guarded tool",
				StopReason: provider.StopToolUse,
				ToolCalls: []provider.ToolCall{{
					ID:   "call_guarded",
					Name: "guarded_tool",
				}},
			}, nil
		}
		return endTurn(`{"status":"should-not-complete"}`)
	}}
	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("guarded step",
			Model("durable/guarded-tool"),
			Tool("guarded_tool", func(ctx context.Context) error {
				if err := store.loseRunLease(ctx); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
					toolCancelled.Store(true)
					return ctx.Err()
				case <-time.After(250 * time.Millisecond):
					return nil
				}
			}, ReadOnly()),
		),
		Reply(JSON[httpTestReply]()),
	})
	rt := httpRuntime{provider: scripted, stateStore: store, durableRuns: cfg}

	_, err := rt.runPlanResultWithTerminal(context.Background(), plan, `{"title":"broken"}`, func(context.Context, planRunResult) error {
		terminalCalls.Add(1)
		return nil
	})

	assertDurableLeaseLoss(t, err)
	if !toolCancelled.Load() {
		t.Fatal("tool context was not cancelled after the run lease was taken over")
	}
	if terminalCalls.Load() != 0 {
		t.Fatalf("terminal calls = %d, want 0 after lease loss", terminalCalls.Load())
	}
	assertFailedDurableRunWithJournal(t, store)
}

func TestDurableRunLeaseRenewErrorCancelsProviderAndSkipsTerminal(t *testing.T) {
	store := newControlledRunLeaseStore(t, runLeaseRenewStoreError)
	cfg := newDurableRunsConfig(0)
	cfg.leaseTTL = 60 * time.Millisecond
	providerCancelled := atomic.Bool{}
	terminalCalls := atomic.Int32{}

	scripted := &leaseContextProvider{complete: func(ctx context.Context, _ provider.Request, _ int) (provider.Response, error) {
		if err := store.loseRunLease(ctx); err != nil {
			return provider.Response{}, err
		}
		select {
		case <-ctx.Done():
			providerCancelled.Store(true)
		case <-time.After(250 * time.Millisecond):
		}
		// Deliberately return a successful completion even after cancellation:
		// the runtime itself must fence the terminal, not trust the provider.
		return endTurn(`{"status":"should-not-complete"}`)
	}}
	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("guarded step", Model("durable/guarded-provider")),
		Reply(JSON[httpTestReply]()),
	})
	rt := httpRuntime{provider: scripted, stateStore: store, durableRuns: cfg}

	_, err := rt.runPlanResultWithTerminal(context.Background(), plan, `{"title":"broken"}`, func(context.Context, planRunResult) error {
		terminalCalls.Add(1)
		return nil
	})

	assertDurableLeaseLoss(t, err)
	if !providerCancelled.Load() {
		t.Fatal("provider context was not cancelled after the lease store renewal error")
	}
	if terminalCalls.Load() != 0 {
		t.Fatalf("terminal calls = %d, want 0 after lease loss", terminalCalls.Load())
	}
	assertFailedDurableRunWithJournal(t, store)
}

func TestDurableRecoveryLeaseLossSkipsWebhookTerminal(t *testing.T) {
	store := newControlledRunLeaseStore(t, runLeaseRenewTakeover)
	cfg := newDurableRecoveryTestConfig(time.Minute)
	cfg.leaseTTL = 60 * time.Millisecond
	providerCancelled := atomic.Bool{}
	webhookCalls := atomic.Int32{}
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(webhook.Close)

	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("recover guarded step", Model("durable/recovery-lease")),
		Push(Webhook(webhook.URL)),
	})
	const execID = "exec_recovery_lease_loss"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_recovery_lease_loss", `{"title":"broken"}`)
	execution, ok, err := store.Execution(context.Background(), execID)
	if err != nil || !ok {
		t.Fatalf("Execution ok=%v err=%v", ok, err)
	}
	journal, ok, err := store.RunJournal(context.Background(), execID)
	if err != nil || !ok {
		t.Fatalf("RunJournal ok=%v err=%v", ok, err)
	}
	holder := cronReplicaID()
	lease, acquired, err := store.AcquireLease(context.Background(), durableRunLeaseName(execID), holder, cfg.leaseTTL)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
	}
	scripted := &leaseContextProvider{complete: func(ctx context.Context, _ provider.Request, _ int) (provider.Response, error) {
		if err := store.loseRunLease(ctx); err != nil {
			return provider.Response{}, err
		}
		select {
		case <-ctx.Done():
			providerCancelled.Store(true)
		case <-time.After(250 * time.Millisecond):
		}
		return endTurn(`{"status":"recovered"}`)
	}}
	eventStream := newDurableRecoveryEventStream(t)
	rt := httpRuntime{
		provider:     scripted,
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		eventStream:  eventStream,
		durableRuns:  cfg,
	}

	rt.recoverClaimedDurableRun(context.Background(), store, []runtimeplan.Plan{plan}, journal, execution, lease, false)

	if !providerCancelled.Load() {
		recorded, eventsErr := store.Events(context.Background(), execID)
		t.Fatalf("recovery provider context was not cancelled after lease takeover (provider calls=%d renew failures=%d webhook calls=%d status=%s events=%+v stream=%+v events_err=%v)",
			scripted.calls.Load(), store.renewFailures.Load(), webhookCalls.Load(), durableExecutionStatus(t, store, execID), recorded, eventStream.List(), eventsErr)
	}
	if webhookCalls.Load() != 0 {
		t.Fatalf("webhook calls = %d, want 0 after recovery lease loss", webhookCalls.Load())
	}
	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), execID); err != nil || !ok {
		t.Fatalf("journal after recovery lease loss ok=%v err=%v, want retained", ok, err)
	}
}

func TestDurableApprovalResumeLeaseLossSkipsWebhookTerminal(t *testing.T) {
	store := newControlledRunLeaseStore(t, runLeaseRenewStoreError)
	cfg := newDurableRunsConfig(0)
	cfg.leaseTTL = 60 * time.Millisecond
	resumeCancelled := atomic.Bool{}
	webhookCalls := atomic.Int32{}
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		webhookCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(webhook.Close)

	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("resume guarded step", Model("durable/resume-lease")),
		Push(Webhook(webhook.URL)),
	})
	session, err := newHTTPPipelineSession(plan)
	if err != nil {
		t.Fatalf("newHTTPPipelineSession returned error: %v", err)
	}
	if err := store.SaveExecution(context.Background(), state.Execution{
		ExecID: session.ExecID, TraceID: session.TraceID, Status: state.ExecutionRunning, StartedAt: session.StartedAt,
	}); err != nil {
		t.Fatalf("SaveExecution returned error: %v", err)
	}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("SaveSession returned error: %v", err)
	}
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID: session.ExecID, PlanKey: durablePlanKey(plan), PlanHash: durablePlanHash(plan), TriggerKind: string(plan.Trigger.Kind), Input: `{"title":"broken"}`,
	}); err != nil {
		t.Fatalf("SaveRunJournal returned error: %v", err)
	}
	rt := httpRuntime{
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		durableRuns:  cfg,
	}
	resume := approvalResume{
		plan:    plan,
		session: session,
		resume: func(ctx context.Context) (planRunResult, error) {
			if err := store.loseRunLease(ctx); err != nil {
				return planRunResult{}, err
			}
			select {
			case <-ctx.Done():
				resumeCancelled.Store(true)
			case <-time.After(250 * time.Millisecond):
			}
			return planRunResult{Output: `{"status":"resumed"}`, Session: session, HasSession: true}, nil
		},
	}

	rt.runApprovedResume(context.Background(), "approval_lease_loss", resume)

	if !resumeCancelled.Load() {
		t.Fatal("approval resume context was not cancelled after lease renewal failed")
	}
	if webhookCalls.Load() != 0 {
		t.Fatalf("webhook calls = %d, want 0 after approval-resume lease loss", webhookCalls.Load())
	}
	if status := durableExecutionStatus(t, store, session.ExecID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed", status)
	}
	if _, ok, err := store.RunJournal(context.Background(), session.ExecID); err != nil || !ok {
		t.Fatalf("journal after approval-resume lease loss ok=%v err=%v, want retained", ok, err)
	}
}

func assertDurableLeaseLoss(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "durable run lease lost") {
		t.Fatalf("error = %v, want durable run lease lost", err)
	}
}

func assertFailedDurableRunWithJournal(t *testing.T, store state.Store) {
	t.Helper()
	executions, err := store.Executions(context.Background())
	if err != nil {
		t.Fatalf("Executions returned error: %v", err)
	}
	if len(executions) != 1 || executions[0].Status != state.ExecutionFailed {
		t.Fatalf("executions = %+v, want one failed execution", executions)
	}
	if _, ok, err := store.RunJournal(context.Background(), executions[0].ExecID); err != nil || !ok {
		t.Fatalf("journal after lease loss ok=%v err=%v, want retained", ok, err)
	}
}
