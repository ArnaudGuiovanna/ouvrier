package ovr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"ouvrier/internal/events"
	runtimeplan "ouvrier/internal/runtime"
	"ouvrier/internal/state"
)

func (rt httpRuntime) emitStreamDeadLetter(ctx context.Context, plan runtimeplan.Plan, result planRunResult, message streamMessage, deliveryErr error) error {
	payload := map[string]any{
		"trigger":  "stream",
		"uri":      streamDisplayURI(plan.Trigger.URI),
		"terminal": string(plan.Terminal.Kind),
		"steps":    len(plan.Steps),
		"error":    deliveryErr.Error(),
		"dlq":      "event_only",
	}
	if id := strings.TrimSpace(message.ID); id != "" {
		payload["id"] = id
	}
	return rt.emitRuntimeEvent(ctx, result, events.EventStreamDeadLettered, payload)
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
			return nil, false, err
		}
		payload["decision"] = "reserved"
		if err := rt.emitSessionEvent(ctx, session, events.EventIdempotencyDecision, payload); err != nil {
			return nil, false, err
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
	duplicate, err := rt.streamReservationCompleted(ctx, existingExecID)
	if err != nil {
		return nil, false, err
	}
	if duplicate {
		payload["decision"] = "duplicate"
	} else {
		payload["decision"] = "retry"
	}
	if err := rt.emitSessionEvent(ctx, eventSession, events.EventIdempotencyDecision, payload); err != nil {
		return nil, false, err
	}
	return &eventSession, duplicate, nil
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
