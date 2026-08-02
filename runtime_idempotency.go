package ovr

import (
	"context"
	"errors"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func (rt httpRuntime) resolveTriggerIdempotency(ctx context.Context, execID, status string) error {
	if rt.stateStore == nil {
		return nil
	}
	outcomes, ok := rt.stateStore.(state.IdempotencyOutcomeStore)
	if !ok {
		// Custom public StateStore implementations keep the legacy reservation
		// contract until they opt into the outcome-aware extension.
		return nil
	}
	outcome := state.IdempotencyFailed
	if status == "completed" {
		outcome = state.IdempotencySucceeded
	}
	return errors.Join(
		outcomes.ResolveIdempotencyByExecution(ctx, execID, "trigger:", outcome),
		outcomes.ResolveIdempotencyByExecution(ctx, execID, "cron_fire:", outcome),
	)
}

func (rt httpRuntime) resolveReservedTriggerFailure(ctx context.Context, session *runtimeplan.Session) error {
	if session == nil {
		return nil
	}
	finishCtx, cancel := newHTTPFinalizationContext(ctx)
	defer cancel()
	return rt.resolveTriggerIdempotency(finishCtx, session.ExecID, "failed")
}
