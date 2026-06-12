package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type recordedIntentEvent struct {
	kind   string // "begin" or "complete"
	intent ToolIntent
}

type fakeIntentRecorder struct {
	events      []recordedIntentEvent
	beginErr    error
	completeErr error
}

func (r *fakeIntentRecorder) BeginToolIntent(_ context.Context, intent ToolIntent) error {
	if r.beginErr != nil {
		return r.beginErr
	}
	r.events = append(r.events, recordedIntentEvent{kind: "begin", intent: intent})
	return nil
}

func (r *fakeIntentRecorder) CompleteToolIntent(_ context.Context, execID, toolCallID string) error {
	if r.completeErr != nil {
		return r.completeErr
	}
	r.events = append(r.events, recordedIntentEvent{kind: "complete", intent: ToolIntent{ExecID: execID, ToolCallID: toolCallID}})
	return nil
}

func intentTestExecutor(t *testing.T) *Executor {
	t.Helper()
	executor := NewExecutor(WithPermissionPolicy(policy.NewDefaultPolicy(policy.AllowSideEffects("email"))))
	if err := executor.Register("send_email", func(ctx context.Context) error { return nil },
		WithMetadata(Metadata{Effect: policy.EffectSideEffecting, SideEffects: []string{"email"}})); err != nil {
		t.Fatalf("Register(send_email) returned error: %v", err)
	}
	if err := executor.Register("lookup", func(ctx context.Context) error { return nil },
		WithMetadata(Metadata{Effect: policy.EffectReadOnly})); err != nil {
		t.Fatalf("Register(lookup) returned error: %v", err)
	}
	if err := executor.Register("failing_tool", func(ctx context.Context) error { return errors.New("boom") },
		WithMetadata(Metadata{Effect: policy.EffectSideEffecting, SideEffects: []string{"email"}})); err != nil {
		t.Fatalf("Register(failing_tool) returned error: %v", err)
	}
	return executor
}

func TestExecutorRecordsIntentAroundNonReadTool(t *testing.T) {
	executor := intentTestExecutor(t)
	recorder := &fakeIntentRecorder{}
	ctx := ContextWithToolIntentRecorder(context.Background(), recorder, "exec_1", 2)

	result, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "send_email", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute result = %+v, want success", result)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("recorder events = %+v, want begin then complete", recorder.events)
	}
	begin := recorder.events[0]
	if begin.kind != "begin" || begin.intent.ExecID != "exec_1" || begin.intent.ToolCallID != "call_1" ||
		begin.intent.StepIndex != 2 || begin.intent.ToolName != "send_email" ||
		begin.intent.Effect != string(policy.EffectSideEffecting) {
		t.Fatalf("begin event = %+v, want exec_1/call_1/step 2/send_email/side_effecting", begin)
	}
	if !strings.HasPrefix(begin.intent.IdemKey, "args:") {
		t.Fatalf("side-effecting idem key = %q, want args hash", begin.intent.IdemKey)
	}
	complete := recorder.events[1]
	if complete.kind != "complete" || complete.intent.ExecID != "exec_1" || complete.intent.ToolCallID != "call_1" {
		t.Fatalf("complete event = %+v", complete)
	}
}

func TestExecutorRecordsNoIntentForReadOnlyTool(t *testing.T) {
	executor := intentTestExecutor(t)
	recorder := &fakeIntentRecorder{}
	ctx := ContextWithToolIntentRecorder(context.Background(), recorder, "exec_1", 0)

	if _, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("recorder events = %+v, want none for read-only tool", recorder.events)
	}
}

func TestExecutorRecordsNoIntentWithoutRecorder(t *testing.T) {
	executor := intentTestExecutor(t)
	if _, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call_1", Name: "send_email", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
}

func TestExecutorCompletesIntentOnToolErrorResult(t *testing.T) {
	// A tool that returned an error still has a definite outcome: the intent
	// must be completed, not left open as indeterminate.
	executor := intentTestExecutor(t)
	recorder := &fakeIntentRecorder{}
	ctx := ContextWithToolIntentRecorder(context.Background(), recorder, "exec_1", 0)

	result, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "failing_tool", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result = %+v, want tool error result", result)
	}
	if len(recorder.events) != 2 || recorder.events[1].kind != "complete" {
		t.Fatalf("recorder events = %+v, want begin then complete", recorder.events)
	}
}

func TestExecutorBeginIntentFailureBlocksExecution(t *testing.T) {
	executor := NewExecutor(WithPermissionPolicy(policy.NewDefaultPolicy(policy.AllowSideEffects("email"))))
	executed := false
	if err := executor.Register("send_email", func(ctx context.Context) error {
		executed = true
		return nil
	}, WithMetadata(Metadata{Effect: policy.EffectSideEffecting, SideEffects: []string{"email"}})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	recorder := &fakeIntentRecorder{beginErr: errors.New("journal unavailable")}
	ctx := ContextWithToolIntentRecorder(context.Background(), recorder, "exec_1", 0)

	result, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "send_email", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result = %+v, want error result when intent cannot be recorded", result)
	}
	if executed {
		t.Fatal("tool executed despite intent write failure; the side effect must not run unrecorded")
	}
}

func TestExecutorCompleteIntentFailureFailsCall(t *testing.T) {
	executor := intentTestExecutor(t)
	recorder := &fakeIntentRecorder{completeErr: errors.New("journal unavailable")}
	ctx := ContextWithToolIntentRecorder(context.Background(), recorder, "exec_1", 0)

	_, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "send_email", Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "complete tool intent") {
		t.Fatalf("Execute error = %v, want complete-intent failure", err)
	}
}

func TestToolIntentIdemKeyMatchesIdempotencyReservation(t *testing.T) {
	executor := NewExecutor()
	if err := executor.Register("charge", func(ctx context.Context, args struct {
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}) error {
		return nil
	}, WithMetadata(Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "payload.id",
	})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	recorder := &fakeIntentRecorder{}
	store := &fakeIdemStore{}
	ctx := ContextWithToolIntentRecorder(context.Background(), recorder, "exec_1", 0)
	ctx = ContextWithIdempotencyStore(ctx, store, "exec_1")

	args := json.RawMessage(`{"payload":{"id":"ord_42"}}`)
	if _, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "charge", Arguments: args}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	wantKey, err := idempotencyReservationKey("charge", "payload.id", args)
	if err != nil {
		t.Fatalf("idempotencyReservationKey returned error: %v", err)
	}
	if len(recorder.events) == 0 || recorder.events[0].intent.IdemKey != wantKey {
		t.Fatalf("intent idem key = %+v, want reservation key %q (must match idempotency.go hashing for #40)", recorder.events, wantKey)
	}
	if len(store.keys) != 1 || store.keys[0] != wantKey {
		t.Fatalf("reserved keys = %v, want %q", store.keys, wantKey)
	}
}

type fakeIdemStore struct {
	keys []string
}

func (s *fakeIdemStore) ReserveIdempotency(_ context.Context, key, execID string) (string, bool, error) {
	s.keys = append(s.keys, key)
	return "", true, nil
}
