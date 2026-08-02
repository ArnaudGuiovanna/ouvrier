package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestDurableRecoveryRecordsTerminalIntentAtTerminalStep(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(webhook.Close)

	store := newDurableTestStore(t)
	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("recover terminal", Model("durable/recovery-terminal-intent")),
		Push(Webhook(webhook.URL)),
	})
	const execID = "exec_recovery_terminal_intent"
	seedInterruptedDurableRun(t, store, plan, execID, "trace_recovery_terminal_intent", `{"title":"broken"}`)
	rt := httpRuntime{
		provider: &durableTestProvider{complete: func(string, int) (provider.Response, error) {
			return endTurn(`{"status":"recovered"}`)
		}},
		toolExecutor: outputAllowedExecutor("webhook"),
		stateStore:   store,
		eventStream:  newDurableRecoveryEventStream(t),
		durableRuns:  newDurableRecoveryTestConfig(time.Minute),
	}

	runDurableRecoveryScanNow(t, rt, plan)

	if status := durableExecutionStatus(t, store, execID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed terminal", status)
	}
	assertDurableTerminalIntent(t, store, execID, len(plan.Steps))
}

func TestDurableApprovalResumeRecordsTerminalIntentAtTerminalStep(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(webhook.Close)

	store := newDurableTestStore(t)
	plan := compileDurableTestPlan(t, []Node{
		From("POST /tickets"),
		Pipe("resume terminal", Model("durable/resume-terminal-intent")),
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
		durableRuns:  newDurableRunsConfig(0),
	}
	resume := approvalResume{
		plan:    plan,
		session: session,
		resume: func(context.Context) (planRunResult, error) {
			return planRunResult{Output: `{"status":"resumed"}`, Session: session, HasSession: true}, nil
		},
	}

	rt.runApprovedResume(context.Background(), "approval_terminal_intent", resume)

	if status := durableExecutionStatus(t, store, session.ExecID); status != state.ExecutionFailed {
		t.Fatalf("execution status = %q, want failed terminal", status)
	}
	assertDurableTerminalIntent(t, store, session.ExecID, len(plan.Steps))
}

func assertDurableTerminalIntent(t *testing.T, store state.Store, execID string, stepIndex int) {
	t.Helper()
	intents, err := store.ToolIntents(context.Background(), execID)
	if err != nil {
		t.Fatalf("ToolIntents returned error: %v", err)
	}
	if len(intents) != 1 || intents[0].ToolName != "ouvrier_push_webhook" || intents[0].StepIndex != stepIndex {
		t.Fatalf("terminal intents = %+v, want webhook intent at step %d", intents, stepIndex)
	}
}
