package ovr

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/harness"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

type suspendedPlanError struct {
	cause  *harness.SuspendedRunError
	resume func(context.Context) (planRunResult, error)
}

func newSuspendedPlanError(cause *harness.SuspendedRunError, resume func(context.Context) (planRunResult, error)) *suspendedPlanError {
	return &suspendedPlanError{cause: cause, resume: resume}
}

func (e *suspendedPlanError) Error() string {
	if e == nil || e.cause == nil {
		return "execution suspended for approval"
	}
	return e.cause.Error()
}

func (e *suspendedPlanError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *suspendedPlanError) Resume(ctx context.Context) (planRunResult, error) {
	if e == nil || e.resume == nil {
		return planRunResult{}, errors.New("suspended plan continuation is unavailable")
	}
	return e.resume(ctx)
}

func suspendedPlan(err error) (*suspendedPlanError, bool) {
	var suspended *suspendedPlanError
	if errors.As(err, &suspended) && suspended != nil {
		return suspended, true
	}
	return nil, false
}

func suspendedTool(err error) (*tools.SuspendedError, bool) {
	var suspended *tools.SuspendedError
	if errors.As(err, &suspended) && suspended != nil {
		return suspended, true
	}
	return nil, false
}

type approvalResume struct {
	plan    runtimeplan.Plan
	session runtimeplan.Session
	resume  func(context.Context) (planRunResult, error)
}

type approvalResumeRegistry struct {
	mu      sync.Mutex
	resumes map[string]approvalResume
}

func newApprovalResumeRegistry() *approvalResumeRegistry {
	return &approvalResumeRegistry{resumes: make(map[string]approvalResume)}
}

func (r *approvalResumeRegistry) Store(approvalID string, resume approvalResume) {
	if r == nil || approvalID == "" || resume.resume == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resumes == nil {
		r.resumes = make(map[string]approvalResume)
	}
	r.resumes[approvalID] = resume
}

func (r *approvalResumeRegistry) Take(approvalID string) (approvalResume, bool) {
	if r == nil || approvalID == "" {
		return approvalResume{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	resume, ok := r.resumes[approvalID]
	if ok {
		delete(r.resumes, approvalID)
	}
	return resume, ok
}

func (rt httpRuntime) rememberSuspendedPlan(err error, plan runtimeplan.Plan, session runtimeplan.Session) bool {
	suspended, ok := suspendedPlan(err)
	if !ok || suspended == nil {
		return false
	}
	toolSuspension, ok := suspendedTool(suspended)
	if !ok {
		return false
	}
	if rt.approvalResumes == nil {
		return false
	}
	rt.approvalResumes.Store(toolSuspension.ApprovalID, approvalResume{
		plan:    plan,
		session: session,
		resume:  suspended.Resume,
	})
	return true
}

func (rt httpRuntime) startApprovedResume(approvalID string) bool {
	if rt.approvalResumes == nil {
		return false
	}
	resume, ok := rt.approvalResumes.Take(approvalID)
	if !ok {
		return false
	}
	if !rt.startAsync(func(ctx context.Context) {
		rt.runApprovedResume(ctx, approvalID, resume)
	}) {
		rt.approvalResumes.Store(approvalID, resume)
		return false
	}
	return true
}

func (rt httpRuntime) runApprovedResume(ctx context.Context, approvalID string, resume approvalResume) {
	executionCtx := ctx
	var runLease *durableRunLeaseHeartbeat
	if rt.durableRuns != nil && rt.stateStore != nil {
		// Re-acquire the run lease for the resumed leg so durable-run
		// recovery never replays this execution while the in-memory resume is
		// live. The brief retry covers the suspend path's deferred release
		// still being in flight; a steady failure means another holder (a
		// recovery claim) owns the lease unexpired — then that holder replays
		// the run with the approval fallback and resuming here would
		// double-execute.
		var err error
		runLease, err = rt.acquireDurableRunLeaseWithRetry(ctx, resume.session.ExecID)
		if err != nil {
			return
		}
		if runLease != nil {
			defer runLease.release()
			executionCtx = runLease.context()
		}
	}
	_ = rt.syncEventStreamWithStore(executionCtx)
	rt.emitApprovalResumeEvent(executionCtx, approvalID, resume, events.EventExecutionResumed, map[string]any{
		"approval_id": approvalID,
	})
	result, err := resume.resume(executionCtx)
	if err != nil {
		err = runLease.executionError(err)
		if !errors.Is(err, errDurableRunLeaseLost) && rt.rememberSuspendedPlan(err, resume.plan, resume.session) {
			return
		}
		payload := map[string]any{
			"approval_id": approvalID,
			"error":       err.Error(),
			"resumed":     true,
		}
		if budget := httpPipelineBudget(err); budget != "" {
			payload["budget"] = budget
		}
		rt.emitApprovalResumeEvent(context.WithoutCancel(executionCtx), approvalID, resume, events.EventPipelineFailed, payload)
		_ = rt.finishPipelineExecution(context.WithoutCancel(executionCtx), resume.session, resume.plan, "failed", err)
		return
	}
	if err := runLease.confirm(executionCtx); err != nil {
		err = runLease.executionError(err)
		rt.emitApprovalResumeEvent(context.WithoutCancel(executionCtx), approvalID, resume, events.EventPipelineFailed, map[string]any{
			"approval_id": approvalID,
			"error":       err.Error(),
			"resumed":     true,
		})
		_ = rt.finishPipelineExecution(context.WithoutCancel(executionCtx), resume.session, resume.plan, "failed", err)
		return
	}
	terminalCtx := executionCtx
	if rt.durableRuns != nil && rt.stateStore != nil {
		terminalJournal := &durableStepJournal{store: rt.stateStore, execID: resume.session.ExecID}
		terminalCtx = terminalJournal.intentContext(executionCtx, len(resume.plan.Steps))
	}
	if err := rt.applyResumedTerminal(terminalCtx, resume.plan, result); err != nil {
		err = runLease.executionError(err)
		rt.emitApprovalResumeEvent(context.WithoutCancel(executionCtx), approvalID, resume, events.EventPipelineFailed, map[string]any{
			"approval_id": approvalID,
			"error":       err.Error(),
			"resumed":     true,
		})
		_ = rt.finishPipelineExecution(context.WithoutCancel(executionCtx), resume.session, resume.plan, "failed", err)
		return
	}
	settleCtx, settleCancel := newHTTPFinalizationContext(executionCtx)
	_ = rt.resolveTriggerIdempotency(settleCtx, resume.session.ExecID, "completed")
	settleCancel()
	if runLease != nil {
		confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(executionCtx), cronLeaseReleaseTimeout)
		err := runLease.stopAndConfirm(confirmCtx)
		cancel()
		if err != nil {
			err = runLease.executionError(err)
			rt.emitApprovalResumeEvent(context.WithoutCancel(executionCtx), approvalID, resume, events.EventPipelineFailed, map[string]any{
				"approval_id": approvalID,
				"error":       err.Error(),
				"resumed":     true,
			})
			_ = rt.finishPipelineExecutionWithTriggerOutcome(context.WithoutCancel(executionCtx), resume.session, resume.plan, "failed", "completed", err)
			return
		}
	}
	_ = rt.finishPipelineExecution(context.WithoutCancel(executionCtx), resume.session, resume.plan, "completed", nil)
}

func (rt httpRuntime) syncEventStreamWithStore(ctx context.Context) error {
	if rt.eventStream == nil || rt.stateStore == nil {
		return nil
	}
	recorded, err := rt.stateStore.EventsSince(ctx, "", 0)
	if err != nil {
		return err
	}
	var maxID uint64
	for _, event := range recorded {
		if event.ID > maxID {
			maxID = event.ID
		}
	}
	rt.eventStream.EnsureNextIDAtLeast(maxID)
	return nil
}

func (rt httpRuntime) emitApprovalResumeEvent(ctx context.Context, approvalID string, resume approvalResume, kind events.EventKind, payload map[string]any) {
	if rt.stateStore == nil && rt.eventStream == nil && rt.hookBus == nil {
		return
	}
	event := events.Event{
		Kind:      kind,
		ExecID:    resume.session.ExecID,
		SessionID: resume.session.SessionID,
		TraceID:   resume.session.TraceID,
		Payload:   payload,
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	event.Payload["approval_id"] = approvalID
	_ = rt.appendRuntimeEvent(ctx, event)
}

func (rt httpRuntime) applyResumedTerminal(ctx context.Context, plan runtimeplan.Plan, result planRunResult) error {
	switch plan.Terminal.Kind {
	case runtimeplan.TerminalReply:
		return rt.validateObservedTerminalReplyOutput(ctx, plan, result)
	case runtimeplan.TerminalPush:
		return rt.applyPushTerminal(ctx, plan.Terminal, result, result.Output)
	case runtimeplan.TerminalSink:
		return rt.applySinkTerminal(ctx, plan.Terminal, result, "output")
	default:
		return fmt.Errorf("%w: terminal missing", errHTTPPipelineIncomplete)
	}
}
