package ovr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/envnames"
	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// durableTestProvider routes scripted completions through a caller-supplied
// function so multi-step pipelines (one model per Pipe) can assert mid-run
// journal state and fail at chosen steps.
type durableTestProvider struct {
	mu       sync.Mutex
	calls    map[string]int
	complete func(model string, call int) (provider.Response, error)
}

func (p *durableTestProvider) Name() string { return "durable-test" }

func (p *durableTestProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	call := p.calls[req.Model]
	p.calls[req.Model]++
	p.mu.Unlock()
	return p.complete(req.Model, call)
}

func newDurableTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	store, err := state.NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func endTurn(text string) (provider.Response, error) {
	return provider.Response{Text: text, StopReason: provider.StopEndTurn}, nil
}

func postDurableTicket(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)
	return rec
}

func soleDurableJournal(t *testing.T, store state.Store) state.RunJournal {
	t.Helper()
	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 1 {
		t.Fatalf("RunJournals = %d entries, want 1: %+v", len(journals), journals)
	}
	return journals[0]
}

// TestDurableRunsCheckpointsEachStepAndPrunesOnSuccess drives a two-step run
// with the flag on: while step two executes, step one's journal row and
// checkpoint must already be durable; once the run succeeds, every journal
// row is pruned.
func TestDurableRunsCheckpointsEachStepAndPrunesOnSuccess(t *testing.T) {
	store := newDurableTestStore(t)

	type midRunState struct {
		journal     state.RunJournal
		checkpoints []state.RunCheckpoint
	}
	var observed *midRunState
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/step1":
			return endTurn(`{"step":"one"}`)
		case "durable/step2":
			// Mid-run: the previous step must already be checkpointed.
			journal := soleDurableJournal(t, store)
			checkpoints, err := store.RunCheckpoints(context.Background(), journal.ExecID)
			if err != nil {
				t.Errorf("mid-run RunCheckpoints returned error: %v", err)
			}
			observed = &midRunState{journal: journal, checkpoints: checkpoints}
			return endTurn(`{"status":"two"}`)
		default:
			return provider.Response{}, fmt.Errorf("unexpected model %q", model)
		}
	}}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("first step", Model("durable/step1")),
		Pipe("second step", Model("durable/step2")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if observed == nil {
		t.Fatal("second step never observed the journal mid-run")
	}
	if observed.journal.PlanKey != "http:POST /tickets" {
		t.Fatalf("journal plan key = %q, want %q", observed.journal.PlanKey, "http:POST /tickets")
	}
	if observed.journal.TriggerKind != "http" {
		t.Fatalf("journal trigger kind = %q, want http", observed.journal.TriggerKind)
	}
	if observed.journal.PlanHash == "" {
		t.Fatal("journal plan hash is empty, want compiled-steps hash")
	}
	if !strings.Contains(observed.journal.Input, `"title"`) {
		t.Fatalf("journal input = %q, want trigger input", observed.journal.Input)
	}
	if len(observed.checkpoints) != 1 {
		t.Fatalf("mid-run checkpoints = %+v, want exactly the completed first step", observed.checkpoints)
	}
	if observed.checkpoints[0].StepIndex != 0 || observed.checkpoints[0].Output != `{"step":"one"}` {
		t.Fatalf("checkpoint = %+v, want step 0 with first-step output", observed.checkpoints[0])
	}

	// On success the whole journal is pruned.
	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 0 {
		t.Fatalf("journals after success = %+v, want pruned", journals)
	}
	checkpoints, err := store.RunCheckpoints(context.Background(), observed.journal.ExecID)
	if err != nil || len(checkpoints) != 0 {
		t.Fatalf("checkpoints after success = %v, %v, want pruned", checkpoints, err)
	}
	intents, err := store.ToolIntents(context.Background(), observed.journal.ExecID)
	if err != nil || len(intents) != 0 {
		t.Fatalf("intents after success = %v, %v, want pruned", intents, err)
	}
}

// TestDurableRunsFailedRunKeepsJournalAndRecordsToolIntents fails the run at
// step two so the journal survives, then asserts step one's checkpoint and
// the completed intent row of its side-effecting tool.
func TestDurableRunsFailedRunKeepsJournalAndRecordsToolIntents(t *testing.T) {
	store := newDurableTestStore(t)
	toolCall := provider.ToolCall{ID: "call_email_1", Name: "send_email"}
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/tool":
			if call == 0 {
				return provider.Response{Text: "sending", StopReason: provider.StopToolUse,
					ToolCalls: []provider.ToolCall{toolCall}}, nil
			}
			return endTurn(`{"step":"one"}`)
		case "durable/fail":
			return provider.Response{}, errors.New("provider exploded")
		default:
			return provider.Response{}, fmt.Errorf("unexpected model %q", model)
		}
	}}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("first step",
			Model("durable/tool"),
			Tool("send_email", func(ctx context.Context) error { return nil }, SideEffecting("email")),
			Tool("lookup", func(ctx context.Context) error { return nil }, ReadOnly()),
		),
		Pipe("second step", Model("durable/fail")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:   scripted,
		stateStore: store,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffects("email")),
		)),
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want failed pipeline; body=%s", rec.Code, rec.Body.String())
	}

	journal := soleDurableJournal(t, store)
	checkpoints, err := store.RunCheckpoints(context.Background(), journal.ExecID)
	if err != nil {
		t.Fatalf("RunCheckpoints returned error: %v", err)
	}
	if len(checkpoints) != 1 || checkpoints[0].StepIndex != 0 || checkpoints[0].Output != `{"step":"one"}` {
		t.Fatalf("checkpoints after failure = %+v, want only completed step 0", checkpoints)
	}

	intents, err := store.ToolIntents(context.Background(), journal.ExecID)
	if err != nil {
		t.Fatalf("ToolIntents returned error: %v", err)
	}
	if len(intents) != 1 {
		t.Fatalf("intents = %+v, want exactly the side-effecting tool (read-only tools never journal)", intents)
	}
	intent := intents[0]
	if intent.ToolCallID != "call_email_1" || intent.ToolName != "send_email" || intent.StepIndex != 0 {
		t.Fatalf("intent = %+v, want send_email call at step 0", intent)
	}
	if intent.Effect != string(policy.EffectSideEffecting) {
		t.Fatalf("intent effect = %q, want side_effecting", intent.Effect)
	}
	if intent.IdemKey == "" {
		t.Fatal("intent idem key is empty, want arguments hash")
	}
	if intent.CompletedAt.IsZero() {
		t.Fatal("intent CompletedAt is zero after the tool returned, want completed")
	}
}

// TestDurableRunsParallelStepCheckpointsAsOneUnit runs Parallel as step 0 and
// fails step 1: exactly one checkpoint exists, for the parallel unit, and no
// sub-branch checkpoints leak in.
func TestDurableRunsParallelStepCheckpointsAsOneUnit(t *testing.T) {
	store := newDurableTestStore(t)
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/quality":
			return endTurn(`{"step":"quality"}`)
		case "durable/compliance":
			return endTurn(`{"step":"compliance"}`)
		case "durable/fail":
			return provider.Response{}, errors.New("provider exploded")
		default:
			return provider.Response{}, fmt.Errorf("unexpected model %q", model)
		}
	}}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("quality", Model("durable/quality")),
			Pipe("compliance", Model("durable/compliance")),
		),
		Pipe("merge", Model("durable/fail")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want failed pipeline; body=%s", rec.Code, rec.Body.String())
	}

	journal := soleDurableJournal(t, store)
	checkpoints, err := store.RunCheckpoints(context.Background(), journal.ExecID)
	if err != nil {
		t.Fatalf("RunCheckpoints returned error: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints = %+v, want exactly one for the parallel unit", checkpoints)
	}
	if checkpoints[0].StepIndex != 0 {
		t.Fatalf("parallel checkpoint step index = %d, want 0", checkpoints[0].StepIndex)
	}
	if !strings.Contains(checkpoints[0].Output, "quality") || !strings.Contains(checkpoints[0].Output, "compliance") {
		t.Fatalf("parallel checkpoint output = %q, want combined branch outputs", checkpoints[0].Output)
	}
}

// TestDurableRunsParallelToolIntentsUseCompositeStepIndex runs Parallel as
// step 0 with a side-effecting tool inside one branch and fails step 1: the
// tool intent must be journaled under the composite step's index, because
// intents flow through the step context even though sub-branches never
// checkpoint individually.
func TestDurableRunsParallelToolIntentsUseCompositeStepIndex(t *testing.T) {
	store := newDurableTestStore(t)
	toolCall := provider.ToolCall{ID: "call_email_1", Name: "send_email"}
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/tool":
			if call == 0 {
				return provider.Response{Text: "sending", StopReason: provider.StopToolUse,
					ToolCalls: []provider.ToolCall{toolCall}}, nil
			}
			return endTurn(`{"step":"tool"}`)
		case "durable/compliance":
			return endTurn(`{"step":"compliance"}`)
		case "durable/fail":
			return provider.Response{}, errors.New("provider exploded")
		default:
			return provider.Response{}, fmt.Errorf("unexpected model %q", model)
		}
	}}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Parallel(
			Pipe("tool branch",
				Model("durable/tool"),
				Tool("send_email", func(ctx context.Context) error { return nil }, SideEffecting("email")),
			),
			Pipe("compliance", Model("durable/compliance")),
		),
		Pipe("merge", Model("durable/fail")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:   scripted,
		stateStore: store,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffects("email")),
		)),
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want failed pipeline; body=%s", rec.Code, rec.Body.String())
	}

	journal := soleDurableJournal(t, store)
	intents, err := store.ToolIntents(context.Background(), journal.ExecID)
	if err != nil {
		t.Fatalf("ToolIntents returned error: %v", err)
	}
	if len(intents) != 1 {
		t.Fatalf("intents = %+v, want exactly the side-effecting branch tool", intents)
	}
	intent := intents[0]
	if intent.ToolCallID != "call_email_1" || intent.ToolName != "send_email" {
		t.Fatalf("intent = %+v, want send_email call from the parallel branch", intent)
	}
	if intent.StepIndex != 0 {
		t.Fatalf("intent step index = %d, want the composite parallel step's index 0", intent.StepIndex)
	}
	if intent.CompletedAt.IsZero() {
		t.Fatal("intent CompletedAt is zero after the tool returned, want completed")
	}
}

// TestDurableRunsResumeCheckpointsAtOriginalPlanIndices suspends step 1 on a
// gated tool, approves it, and lets the resumed run fail at step 3: the
// checkpoints written before the suspend, by the resumed step, and by the
// steps after it (the withBase offset path) must all land at the original
// plan indices 0, 1, and 2.
func TestDurableRunsResumeCheckpointsAtOriginalPlanIndices(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	store := newDurableTestStore(t)
	toolCall := provider.ToolCall{ID: "call_wire_1", Name: "wire_payment"}
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/step0":
			return endTurn(`{"step":"zero"}`)
		case "durable/gated":
			if call == 0 {
				return provider.Response{Text: "need approval", StopReason: provider.StopToolUse,
					ToolCalls: []provider.ToolCall{toolCall}}, nil
			}
			return endTurn(`{"step":"gated"}`)
		case "durable/step2":
			return endTurn(`{"step":"two"}`)
		case "durable/fail":
			return provider.Response{}, errors.New("provider exploded")
		default:
			return provider.Response{}, fmt.Errorf("unexpected model %q", model)
		}
	}}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("step zero", Model("durable/step0")),
		Pipe("gated step",
			Model("durable/gated"),
			Tool("wire_payment", func(ctx context.Context) error { return nil }, RequiresApproval()),
		),
		Pipe("step two", Model("durable/step2")),
		Pipe("failing step", Model("durable/fail")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want suspended run; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status     string `json:"status"`
		ApprovalID string `json:"approval_id"`
		ExecID     string `json:"exec_id"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "pending_approval" || body.ApprovalID == "" || body.ExecID == "" {
		t.Fatalf("body = %+v, want pending_approval with approval and exec ids", body)
	}

	approvalReq := httptest.NewRequest(http.MethodPost, "/admin/approvals/"+body.ApprovalID,
		strings.NewReader(`{"decision":"approve","decided_by":"ops"}`))
	approvalRec := httptest.NewRecorder()
	handler.ServeHTTP(approvalRec, approvalReq)
	if approvalRec.Code != http.StatusOK {
		t.Fatalf("approval status = %d body=%s, want %d", approvalRec.Code, approvalRec.Body.String(), http.StatusOK)
	}

	// The resume runs asynchronously; the run fails at step 3, which keeps the
	// journal so the checkpoint indices can be asserted.
	deadline := time.Now().Add(2 * time.Second)
	for {
		execution, ok, err := store.Execution(context.Background(), body.ExecID)
		if err != nil {
			t.Fatalf("Execution returned error: %v", err)
		}
		if ok && execution.Status == state.ExecutionFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution = %+v ok=%v, want failed after resumed run", execution, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	checkpoints, err := store.RunCheckpoints(context.Background(), body.ExecID)
	if err != nil {
		t.Fatalf("RunCheckpoints returned error: %v", err)
	}
	if len(checkpoints) != 3 {
		t.Fatalf("checkpoints = %+v, want steps 0, 1, 2 at their original plan indices", checkpoints)
	}
	wantOutputs := map[int]string{0: `{"step":"zero"}`, 1: `{"step":"gated"}`, 2: `{"step":"two"}`}
	for i, checkpoint := range checkpoints {
		if checkpoint.StepIndex != i {
			t.Fatalf("checkpoint[%d] step index = %d, want %d (resume must keep original plan indices)", i, checkpoint.StepIndex, i)
		}
		if checkpoint.Output != wantOutputs[i] {
			t.Fatalf("checkpoint[%d] output = %q, want %q", i, checkpoint.Output, wantOutputs[i])
		}
	}
}

// TestDurableRunsFlagOffWritesNothing runs the same tool-using pipeline with
// the flag off and asserts zero journal rows by inspecting the database.
func TestDurableRunsFlagOffWritesNothing(t *testing.T) {
	store := newDurableTestStore(t)
	toolCall := provider.ToolCall{ID: "call_email_1", Name: "send_email"}
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		switch model {
		case "durable/tool":
			if call == 0 {
				return provider.Response{Text: "sending", StopReason: provider.StopToolUse,
					ToolCalls: []provider.ToolCall{toolCall}}, nil
			}
			return endTurn(`{"step":"one"}`)
		case "durable/step2":
			return endTurn(`{"status":"done"}`)
		default:
			return provider.Response{}, fmt.Errorf("unexpected model %q", model)
		}
	}}

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("first step",
			Model("durable/tool"),
			Tool("send_email", func(ctx context.Context) error { return nil }, SideEffecting("email")),
		),
		Pipe("second step", Model("durable/step2")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:   scripted,
		stateStore: store,
		toolExecutor: tools.NewExecutor(tools.WithPermissionPolicy(
			policy.NewDefaultPolicy(policy.AllowSideEffects("email")),
		)),
		// durableRuns deliberately nil: flag off.
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 0 {
		t.Fatalf("journals with flag off = %+v, want zero writes", journals)
	}
	executions, err := store.Executions(context.Background())
	if err != nil || len(executions) == 0 {
		t.Fatalf("Executions = %v, %v, want the recorded run", executions, err)
	}
	for _, execution := range executions {
		checkpoints, err := store.RunCheckpoints(context.Background(), execution.ExecID)
		if err != nil || len(checkpoints) != 0 {
			t.Fatalf("checkpoints with flag off = %v, %v, want zero writes", checkpoints, err)
		}
		intents, err := store.ToolIntents(context.Background(), execution.ExecID)
		if err != nil || len(intents) != 0 {
			t.Fatalf("intents with flag off = %v, %v, want zero writes", intents, err)
		}
	}
}

// TestDurableRunsRetentionPrunesExpiredFailedJournals seeds a journal older
// than the retention window; the sweep in finishPipelineExecution prunes it
// when an unrelated run finishes.
func TestDurableRunsRetentionPrunesExpiredFailedJournals(t *testing.T) {
	store := newDurableTestStore(t)
	for _, execution := range []state.Execution{
		{ExecID: "exec_expired", Status: state.ExecutionFailed, StartedAt: time.Now().UTC().Add(-100 * time.Hour), CompletedAt: time.Now().UTC().Add(-99 * time.Hour)},
		{ExecID: "exec_recent_failure", Status: state.ExecutionFailed, StartedAt: time.Now().UTC().Add(-time.Hour), CompletedAt: time.Now().UTC()},
	} {
		if err := store.SaveExecution(context.Background(), execution); err != nil {
			t.Fatalf("seed SaveExecution(%s) returned error: %v", execution.ExecID, err)
		}
	}
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID:    "exec_expired",
		PlanKey:   "http:POST /tickets",
		CreatedAt: time.Now().UTC().Add(-100 * time.Hour),
	}); err != nil {
		t.Fatalf("seed SaveRunJournal returned error: %v", err)
	}
	if err := store.SaveRunJournal(context.Background(), state.RunJournal{
		ExecID:    "exec_recent_failure",
		PlanKey:   "http:POST /tickets",
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed SaveRunJournal returned error: %v", err)
	}

	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		return endTurn(`{"status":"done"}`)
	}}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/step1")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		durableRuns: newDurableRunsConfig(72 * time.Hour),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	journals, err := store.RunJournals(context.Background())
	if err != nil {
		t.Fatalf("RunJournals returned error: %v", err)
	}
	if len(journals) != 1 || journals[0].ExecID != "exec_recent_failure" {
		t.Fatalf("journals after retention sweep = %+v, want only exec_recent_failure (expired pruned, recent kept)", journals)
	}
}

// failingPruneStore makes every prune fail so the failure surfaces through
// the event stream and /admin/health.
type failingPruneStore struct {
	state.Store
}

func (s failingPruneStore) PruneRunJournal(context.Context, string) error {
	return errors.New("disk on fire")
}

func (s failingPruneStore) PruneRunJournalsBefore(context.Context, time.Time) ([]string, error) {
	return nil, errors.New("disk on fire")
}

func TestDurableRunsPruneFailureEmitsEventAndSurfacesInHealth(t *testing.T) {
	store := failingPruneStore{Store: newDurableTestStore(t)}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &durableTestProvider{complete: func(model string, call int) (provider.Response, error) {
		return endTurn(`{"status":"done"}`)
	}}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("only step", Model("durable/step1")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{
		provider:    scripted,
		stateStore:  store,
		eventStream: stream,
		adminToken:  "secret",
		durableRuns: newDurableRunsConfig(0),
	})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := postDurableTicket(t, handler)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want completed run despite prune failure; body=%s", rec.Code, rec.Body.String())
	}

	recorded, err := store.Events(context.Background(), "")
	if err != nil {
		t.Fatalf("Events returned error: %v", err)
	}
	pruneFailures := 0
	for _, event := range recorded {
		if event.Kind == events.EventDurableRunPruneFailed {
			pruneFailures++
			if msg, _ := event.Payload["error"].(string); !strings.Contains(msg, "disk on fire") {
				t.Fatalf("prune failure payload = %+v, want error message", event.Payload)
			}
		}
	}
	if pruneFailures == 0 {
		t.Fatal("no durable_run_prune_failed event emitted")
	}

	healthRec := httptest.NewRecorder()
	healthReq := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	healthReq.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("health status = %d; body=%s", healthRec.Code, healthRec.Body.String())
	}
	body := healthRec.Body.String()
	if !strings.Contains(body, `"durable_runs"`) {
		t.Fatalf("health body = %s, want durable_runs section", body)
	}
	if !strings.Contains(body, "disk on fire") {
		t.Fatalf("health body = %s, want last_prune_error surfaced", body)
	}
	if !strings.Contains(body, `"prune_failures":2`) {
		t.Fatalf("health body = %s, want both prune failures counted (completed run + retention sweep)", body)
	}
}

func TestDurableRunsMemoryBackendStartupFails(t *testing.T) {
	t.Setenv(envnames.StateBackend, state.BackendMemory)
	t.Setenv(envnames.DurableRuns, "1")

	_, _, err := defaultHTTPRuntimeForRun()
	if err == nil {
		t.Fatal("defaultHTTPRuntimeForRun returned nil error for memory backend with durable runs on")
	}
	for _, want := range []string{envnames.DurableRuns, state.BackendMemory, state.BackendSQLite, state.BackendPostgres} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("startup error %q does not mention %q (must be actionable)", err.Error(), want)
		}
	}
}

func TestDurableRunsSQLiteBackendStartupSucceeds(t *testing.T) {
	t.Setenv(envnames.StateBackend, state.BackendSQLite)
	t.Setenv(envnames.StatePath, filepath.Join(t.TempDir(), "state.db"))
	t.Setenv(envnames.DurableRuns, "1")
	t.Setenv(envnames.DurableRetention, "24h")

	rt, closeRuntime, err := defaultHTTPRuntimeForRun()
	if err != nil {
		t.Fatalf("defaultHTTPRuntimeForRun returned error: %v", err)
	}
	defer func() { _ = closeRuntime() }()
	if rt.durableRuns == nil {
		t.Fatal("durableRuns config is nil with the flag on")
	}
	if rt.durableRuns.retention != 24*time.Hour {
		t.Fatalf("retention = %s, want 24h from %s", rt.durableRuns.retention, envnames.DurableRetention)
	}
}

func TestDurableRunsFlagOffLeavesRuntimeUnconfigured(t *testing.T) {
	t.Setenv(envnames.StateBackend, state.BackendSQLite)
	t.Setenv(envnames.StatePath, filepath.Join(t.TempDir(), "state.db"))
	t.Setenv(envnames.DurableRuns, "")

	rt, closeRuntime, err := defaultHTTPRuntimeForRun()
	if err != nil {
		t.Fatalf("defaultHTTPRuntimeForRun returned error: %v", err)
	}
	defer func() { _ = closeRuntime() }()
	if rt.durableRuns != nil {
		t.Fatal("durableRuns config is non-nil with the flag unset, want off by default")
	}
}

func TestDurableRunsRejectsCustomPublicStateStore(t *testing.T) {
	t.Setenv(envnames.DurableRuns, "1")
	_, err := durableRunsConfigForStore(publicStateStoreAdapter{})
	if err == nil || !strings.Contains(err.Error(), "WithStateStore") {
		t.Fatalf("durableRunsConfigForStore error = %v, want custom-store refusal", err)
	}
}

func TestDurableRunsEnvParsing(t *testing.T) {
	t.Setenv(envnames.DurableRuns, "maybe")
	if _, err := durableRunsEnabledFromEnv(); err == nil || !strings.Contains(err.Error(), envnames.DurableRuns) {
		t.Fatalf("durableRunsEnabledFromEnv error = %v, want actionable parse failure", err)
	}
	t.Setenv(envnames.DurableRuns, "0")
	if enabled, err := durableRunsEnabledFromEnv(); err != nil || enabled {
		t.Fatalf("durableRunsEnabledFromEnv(0) = %v, %v, want off", enabled, err)
	}

	t.Setenv(envnames.DurableRetention, "not-a-duration")
	if _, err := durableRetentionFromEnv(); err == nil || !strings.Contains(err.Error(), envnames.DurableRetention) {
		t.Fatalf("durableRetentionFromEnv error = %v, want actionable parse failure", err)
	}
	t.Setenv(envnames.DurableRetention, "")
	if retention, err := durableRetentionFromEnv(); err != nil || retention != defaultDurableRetention {
		t.Fatalf("default retention = %v, %v, want %s", retention, err, defaultDurableRetention)
	}
}

func TestDurablePlanHashIsDeterministicAndStructureSensitive(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("first step", Model("durable/step1")),
		Pipe("second step", Model("durable/step2")),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	first := durablePlanHash(plans[0])
	if first == "" {
		t.Fatal("plan hash is empty")
	}
	if again := durablePlanHash(plans[0]); again != first {
		t.Fatalf("plan hash not deterministic: %q vs %q", first, again)
	}

	edited, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("first step EDITED", Model("durable/step1")),
		Pipe("second step", Model("durable/step2")),
		Reply(JSON[httpTestReply]()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if durablePlanHash(edited[0]) == first {
		t.Fatal("plan hash unchanged after editing a step goal, want mismatch for #40 abandonment")
	}

	changedTerminal := plans[0]
	changedTerminal.Terminal.PushWebhookURL = "https://new.example.invalid/result"
	if durablePlanHash(changedTerminal) == first {
		t.Fatal("plan hash unchanged after editing the terminal destination, want fail-closed recovery")
	}
}

func TestDurablePlanHashBindsWorkerBuildIdentity(t *testing.T) {
	plans, err := compilePlans([]Node{
		From("POST /tickets"),
		Pipe("same compiled plan", Model("durable/build-binding")),
		Reply(Accepted()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	releaseA := durablePlanHashWithBuildIdentity(plans[0], "sha256:release-a")
	if again := durablePlanHashWithBuildIdentity(plans[0], "sha256:release-a"); again != releaseA {
		t.Fatalf("same plan and build identity produced %q then %q", releaseA, again)
	}
	if releaseB := durablePlanHashWithBuildIdentity(plans[0], "sha256:release-b"); releaseB == releaseA {
		t.Fatal("plan hash did not change with the worker build identity")
	}
}
