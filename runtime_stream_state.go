package ovr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

var errStreamIdempotencyPending = errors.New("stream idempotency reservation is still pending")

func (rt httpRuntime) emitStreamDeadLetter(ctx context.Context, plan runtimeplan.Plan, result planRunResult, message streamMessage, deliveryErr error, attempt int) error {
	dlq := "event_only"
	if target := strings.TrimSpace(plan.Trigger.DLQTarget); target != "" {
		dlq = streamDisplayURI(target)
	}
	payload := map[string]any{
		"trigger":  "stream",
		"uri":      streamDisplayURI(plan.Trigger.URI),
		"terminal": string(plan.Terminal.Kind),
		"steps":    len(plan.Steps),
		"error":    deliveryErr.Error(),
		"reason":   deliveryErr.Error(),
		"dlq":      dlq,
	}
	if attempt > 0 {
		payload["attempt"] = attempt
	}
	if id := strings.TrimSpace(message.ID); id != "" {
		payload["id"] = id
	}
	return rt.emitRuntimeEvent(ctx, result, events.EventStreamDeadLettered, payload)
}

func (rt httpRuntime) emitStreamRedelivered(ctx context.Context, plan runtimeplan.Plan, result planRunResult, message streamMessage, deliveryErr error, attempt int) error {
	payload := map[string]any{
		"trigger": "stream",
		"uri":     streamDisplayURI(plan.Trigger.URI),
		"reason":  deliveryErr.Error(),
		"error":   deliveryErr.Error(),
		"attempt": attempt,
	}
	if id := strings.TrimSpace(message.ID); id != "" {
		payload["id"] = id
	}
	return rt.emitRuntimeEvent(ctx, result, events.EventStreamRedelivered, payload)
}

func (rt httpRuntime) reserveStreamIdempotency(ctx context.Context, plan runtimeplan.Plan, message streamMessage) (*runtimeplan.Session, bool, error) {
	id := strings.TrimSpace(message.ID)
	if id == "" || rt.stateStore == nil {
		return nil, false, nil
	}
	session, err := newHTTPPipelineSession(plan)
	if err != nil {
		return nil, false, err
	}

	key := streamIdempotencyReservationKey(plan, id)
	retrying := false
	if outcomes, ok := rt.stateStore.(state.IdempotencyOutcomeStore); ok {
		if record, found, lookupErr := outcomes.Idempotency(ctx, key); lookupErr != nil {
			return nil, false, lookupErr
		} else if found && record.Outcome == state.IdempotencyFailed {
			retrying = true
		}
	}
	existingExecID, reserved, err := rt.stateStore.ReserveIdempotency(ctx, key, session.ExecID)
	if err != nil {
		return nil, false, err
	}
	payload := map[string]any{
		"scope":   "trigger",
		"trigger": "stream",
		"uri":     streamDisplayURI(plan.Trigger.URI),
	}
	if reserved {
		if err := rt.stateStore.SaveSession(ctx, session); err != nil {
			return nil, false, resolveStreamReservationFailure(ctx, rt.stateStore, key, session.ExecID, err)
		}
		payload["decision"] = "reserved"
		if retrying {
			payload["decision"] = "retry"
		}
		if err := rt.emitSessionEvent(ctx, session, events.EventIdempotencyDecision, payload); err != nil {
			return nil, false, resolveStreamReservationFailure(ctx, rt.stateStore, key, session.ExecID, err)
		}
		return &session, false, nil
	}
	payload["existing_exec_id"] = existingExecID
	existingSession, foundSession, err := rt.streamSessionForExec(ctx, existingExecID)
	if err != nil {
		return nil, false, err
	}
	eventSession := session
	if foundSession {
		eventSession = existingSession
	}
	duplicate := false
	pending := false
	if outcomes, ok := rt.stateStore.(state.IdempotencyOutcomeStore); ok {
		record, found, lookupErr := outcomes.Idempotency(ctx, key)
		if lookupErr != nil {
			return nil, false, lookupErr
		}
		if !found {
			return nil, false, errors.New("stream idempotency reservation disappeared after conflict")
		}
		switch record.Outcome {
		case state.IdempotencySucceeded:
			duplicate = true
		case state.IdempotencyPending:
			pending = true
		case state.IdempotencyFailed:
			return nil, false, errors.New("failed stream idempotency reservation was not made available for retry")
		default:
			return nil, false, fmt.Errorf("unknown stream idempotency outcome %q", record.Outcome)
		}
	} else {
		// Legacy custom stores do not expose reservation outcomes. A completed
		// execution is safe to deduplicate; every other state is treated as
		// in-flight and fails closed because ownership cannot be transferred
		// atomically through the legacy interface.
		duplicate, err = rt.streamReservationCompleted(ctx, existingExecID)
		if err != nil {
			return nil, false, err
		}
		pending = !duplicate
	}
	if duplicate {
		payload["decision"] = "duplicate"
	} else if pending {
		payload["decision"] = "in_progress"
	}
	if err := rt.emitSessionEvent(ctx, eventSession, events.EventIdempotencyDecision, payload); err != nil {
		return nil, false, err
	}
	if pending {
		return &eventSession, false, fmt.Errorf("%w for execution %s", errStreamIdempotencyPending, existingExecID)
	}
	return &eventSession, duplicate, nil
}

func resolveStreamReservationFailure(ctx context.Context, store state.Store, key, execID string, cause error) error {
	outcomes, ok := store.(state.IdempotencyOutcomeStore)
	if !ok {
		return cause
	}
	resolveCtx := context.WithoutCancel(ctx)
	return errors.Join(cause, outcomes.ResolveIdempotency(resolveCtx, key, execID, state.IdempotencyFailed))
}

func (rt httpRuntime) streamReservationCompleted(ctx context.Context, execID string) (bool, error) {
	execution, ok, err := rt.stateStore.Execution(ctx, execID)
	if err != nil {
		return false, err
	}
	return ok && execution.Status == state.ExecutionCompleted, nil
}

func (rt httpRuntime) streamSessionForExec(ctx context.Context, execID string) (runtimeplan.Session, bool, error) {
	sessions, err := rt.stateStore.Sessions(ctx)
	if err != nil {
		return runtimeplan.Session{}, false, err
	}
	for _, session := range sessions {
		if session.ExecID == execID {
			return session, true, nil
		}
	}
	return runtimeplan.Session{}, false, nil
}

func streamIdempotencyReservationKey(plan runtimeplan.Plan, id string) string {
	sum := sha256.Sum256([]byte(plan.Trigger.URI + "\x00" + id))
	return strings.Join([]string{
		"trigger",
		"stream",
		"stream-message-id",
		hex.EncodeToString(sum[:]),
	}, ":")
}
