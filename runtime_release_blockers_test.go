package ovr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestRuntimeEventPersistenceAllocatesCollisionFreeIDsAcrossReplicas(t *testing.T) {
	const perReplica = 32
	store := state.NewMemoryStore()
	streamA, err := events.NewEventStream(events.WithRetentionLimit(perReplica))
	if err != nil {
		t.Fatalf("NewEventStream(A) returned error: %v", err)
	}
	streamB, err := events.NewEventStream(events.WithRetentionLimit(perReplica))
	if err != nil {
		t.Fatalf("NewEventStream(B) returned error: %v", err)
	}
	replicas := []httpRuntime{
		{stateStore: store, eventStream: streamA},
		{stateStore: store, eventStream: streamB},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for replicaIndex, replica := range replicas {
		for eventIndex := 0; eventIndex < perReplica; eventIndex++ {
			wg.Add(1)
			go func(replicaIndex, eventIndex int, replica httpRuntime) {
				defer wg.Done()
				<-start
				if err := replica.appendRuntimeEvent(context.Background(), events.Event{
					Kind:   events.EventBeforeTool,
					ExecID: fmt.Sprintf("exec_%d", replicaIndex),
					Payload: map[string]any{
						"event": eventIndex,
					},
				}); err != nil {
					t.Errorf("replica %d event %d: %v", replicaIndex, eventIndex, err)
				}
			}(replicaIndex, eventIndex, replica)
		}
	}
	close(start)
	wg.Wait()

	recorded, err := store.EventsSince(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("EventsSince returned error: %v", err)
	}
	if len(recorded) != len(replicas)*perReplica {
		t.Fatalf("persisted events = %d, want %d", len(recorded), len(replicas)*perReplica)
	}
	for i, event := range recorded {
		if event.ID != uint64(i+1) {
			t.Fatalf("persisted event[%d].ID = %d, want %d", i, event.ID, i+1)
		}
	}
	localIDs := make(map[uint64]struct{}, len(recorded))
	for _, stream := range []*events.EventStream{streamA, streamB} {
		for _, event := range stream.List() {
			if _, duplicate := localIDs[event.ID]; duplicate {
				t.Fatalf("durable event ID %d appeared in both replica streams", event.ID)
			}
			localIDs[event.ID] = struct{}{}
		}
	}
	if len(localIDs) != len(recorded) {
		t.Fatalf("replica stream IDs = %d, want %d globally unique IDs", len(localIDs), len(recorded))
	}
}

func TestHTTPTriggerReservationFailsClosedWhenPipelineStartFails(t *testing.T) {
	store := &failRunningExecutionStore{MemoryStore: state.NewMemoryStore()}
	plan := runtimeplan.Plan{
		Trigger:  runtimeplan.Trigger{Kind: runtimeplan.TriggerHTTP, Method: "POST", Path: "/tickets"},
		Terminal: runtimeplan.Terminal{Kind: runtimeplan.TerminalReply},
	}
	session, err := newHTTPPipelineSession(plan)
	if err != nil {
		t.Fatalf("newHTTPPipelineSession returned error: %v", err)
	}
	const key = "trigger:early-failure"
	if _, reserved, err := store.ReserveIdempotency(context.Background(), key, session.ExecID); err != nil || !reserved {
		t.Fatalf("ReserveIdempotency reserved=%v err=%v", reserved, err)
	}

	rt := httpRuntime{stateStore: store}
	if _, err := rt.runPlanResultWithSessionAndTerminal(context.Background(), plan, "input", &session, nil); err == nil {
		t.Fatal("runPlanResultWithSessionAndTerminal returned nil error, want start failure")
	}
	record, ok, err := store.Idempotency(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Idempotency ok=%v err=%v", ok, err)
	}
	if record.Outcome != state.IdempotencyFailed {
		t.Fatalf("idempotency outcome = %q, want failed", record.Outcome)
	}
}

func TestHTTPTriggerReservationFailsWhenDecisionEventIsCancelled(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	store := &cancelingEventStore{MemoryStore: state.NewMemoryStore(), cancel: cancelRequest}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plan := runtimeplan.Plan{
		Trigger: runtimeplan.Trigger{
			Kind: runtimeplan.TriggerHTTP, Method: "POST", Path: "/tickets", IdempotencyHeader: "X-Delivery-ID",
		},
		Terminal: runtimeplan.Terminal{Kind: runtimeplan.TerminalReply},
	}
	req := httptest.NewRequest("POST", "/tickets", nil).WithContext(requestCtx)
	req.Header.Set("X-Delivery-ID", "delivery-1")
	recorder := httptest.NewRecorder()
	rt := httpRuntime{stateStore: store, eventStream: stream}
	if _, _, ok := rt.reserveTriggerIdempotency(recorder, req, plan); ok {
		t.Fatal("reserveTriggerIdempotency ok = true, want event persistence failure")
	}

	key := triggerIdempotencyReservationKey(plan, "X-Delivery-ID", "delivery-1")
	record, found, err := store.Idempotency(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("Idempotency found=%v err=%v", found, err)
	}
	if record.Outcome != state.IdempotencyFailed {
		t.Fatalf("idempotency outcome = %q, want failed", record.Outcome)
	}
}

func TestHTTPFinalizationSurvivesRequestCancellationAfterTerminalSuccess(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plan := runtimeplan.Plan{
		Trigger:  runtimeplan.Trigger{Kind: runtimeplan.TriggerHTTP, Method: "POST", Path: "/tickets"},
		Terminal: runtimeplan.Terminal{Kind: runtimeplan.TerminalPush},
	}
	session, err := newHTTPPipelineSession(plan)
	if err != nil {
		t.Fatalf("newHTTPPipelineSession returned error: %v", err)
	}
	const key = "trigger:terminal-success"
	if _, reserved, err := store.ReserveIdempotency(context.Background(), key, session.ExecID); err != nil || !reserved {
		t.Fatalf("ReserveIdempotency reserved=%v err=%v", reserved, err)
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	rt := httpRuntime{stateStore: store, eventStream: stream}
	_, err = rt.runPlanResultWithSessionAndTerminal(requestCtx, plan, "input", &session, func(context.Context, planRunResult) error {
		cancelRequest()
		return nil
	})
	if err != nil {
		t.Fatalf("runPlanResultWithSessionAndTerminal returned error after successful terminal: %v", err)
	}
	record, ok, err := store.Idempotency(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Idempotency ok=%v err=%v", ok, err)
	}
	if record.Outcome != state.IdempotencySucceeded {
		t.Fatalf("idempotency outcome = %q, want succeeded", record.Outcome)
	}
	execution, ok, err := store.Execution(context.Background(), session.ExecID)
	if err != nil || !ok {
		t.Fatalf("Execution ok=%v err=%v", ok, err)
	}
	if execution.Status != state.ExecutionCompleted {
		t.Fatalf("execution status = %q, want completed", execution.Status)
	}
}

func TestHTTPFinalizationKeepsSuccessfulTerminalIdempotentWhenExecutionSnapshotFails(t *testing.T) {
	store := &failCompletedExecutionStore{MemoryStore: state.NewMemoryStore()}
	plan := runtimeplan.Plan{
		Trigger:  runtimeplan.Trigger{Kind: runtimeplan.TriggerHTTP, Method: "POST", Path: "/tickets"},
		Terminal: runtimeplan.Terminal{Kind: runtimeplan.TerminalPush},
	}
	session, err := newHTTPPipelineSession(plan)
	if err != nil {
		t.Fatalf("newHTTPPipelineSession returned error: %v", err)
	}
	const key = "trigger:terminal-success-save-failure"
	if _, reserved, err := store.ReserveIdempotency(context.Background(), key, session.ExecID); err != nil || !reserved {
		t.Fatalf("ReserveIdempotency reserved=%v err=%v", reserved, err)
	}

	rt := httpRuntime{stateStore: store}
	if _, err := rt.runPlanResultWithSessionAndTerminal(context.Background(), plan, "input", &session, func(context.Context, planRunResult) error {
		return nil
	}); err == nil {
		t.Fatal("runPlanResultWithSessionAndTerminal returned nil error, want final snapshot failure")
	}
	record, ok, err := store.Idempotency(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Idempotency ok=%v err=%v", ok, err)
	}
	if record.Outcome != state.IdempotencySucceeded {
		t.Fatalf("idempotency outcome = %q, want succeeded despite later snapshot failure", record.Outcome)
	}
}

func TestSSEFallsBackToDurableStoreAfterLocalRetentionGap(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream(events.WithRetentionLimit(1))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	rt := httpRuntime{stateStore: store, eventStream: stream}
	for i := 0; i < 3; i++ {
		if err := rt.appendRuntimeEvent(context.Background(), events.Event{
			Kind: events.EventLLMTokenDelta, ExecID: "exec_sse", Payload: map[string]any{"delta": fmt.Sprint(i)},
		}); err != nil {
			t.Fatalf("appendRuntimeEvent(%d) returned error: %v", i, err)
		}
	}

	var output bytes.Buffer
	cursor, err := rt.writeSSEEventsSince(context.Background(), &output, "exec_sse", 0)
	if err != nil {
		t.Fatalf("writeSSEEventsSince returned error: %v", err)
	}
	if cursor != 3 {
		t.Fatalf("cursor = %d, want 3", cursor)
	}
	if got := bytes.Count(output.Bytes(), []byte("event: llm_token_delta\n")); got != 3 {
		t.Fatalf("SSE delta count = %d, want 3; output=%s", got, output.String())
	}
}

func TestSSEReportsExplicitGapWithoutDurableStore(t *testing.T) {
	stream, err := events.NewEventStream(events.WithRetentionLimit(1))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := stream.Append(context.Background(), events.Event{Kind: events.EventLLMTokenDelta, ExecID: "exec_sse"}); err != nil {
			t.Fatalf("Append(%d) returned error: %v", i, err)
		}
	}

	var output bytes.Buffer
	cursor, err := (httpRuntime{eventStream: stream}).writeSSEEventsSince(context.Background(), &output, "exec_sse", 0)
	if !errors.Is(err, events.ErrEventHistoryGap) {
		t.Fatalf("writeSSEEventsSince error = %v, want ErrEventHistoryGap", err)
	}
	if cursor != 0 || output.Len() != 0 {
		t.Fatalf("cursor=%d output=%q, want no silent partial delivery", cursor, output.String())
	}
}

func TestSSEUsesHealthyLocalWindowWithoutQueryingDurableHistory(t *testing.T) {
	store := &failingEventsSinceStore{MemoryStore: state.NewMemoryStore()}
	stream, err := events.NewEventStream(events.WithRetentionLimit(4))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	rt := httpRuntime{stateStore: store, eventStream: stream}
	if err := rt.appendRuntimeEvent(context.Background(), events.Event{
		Kind: events.EventLLMTokenDelta, ExecID: "exec_sse",
	}); err != nil {
		t.Fatalf("appendRuntimeEvent returned error: %v", err)
	}

	var output bytes.Buffer
	cursor, err := rt.writeSSEEventsSince(context.Background(), &output, "exec_sse", 0)
	if err != nil {
		t.Fatalf("writeSSEEventsSince returned error despite healthy local window: %v", err)
	}
	if cursor != 1 || output.Len() == 0 {
		t.Fatalf("cursor=%d output=%q, want locally delivered event 1", cursor, output.String())
	}
}

func TestSSEDoesNotAdvanceCursorWhenWriterFails(t *testing.T) {
	store := state.NewMemoryStore()
	if _, err := store.AddEvent(context.Background(), events.Event{Kind: events.EventLLMTokenDelta, ExecID: "exec_sse"}); err != nil {
		t.Fatalf("AddEvent returned error: %v", err)
	}
	cursor, err := (httpRuntime{stateStore: store}).writeSSEEventsSince(context.Background(), failingSSEWriter{}, "exec_sse", 0)
	if err == nil {
		t.Fatal("writeSSEEventsSince returned nil error, want writer failure")
	}
	if cursor != 0 {
		t.Fatalf("cursor = %d, want 0 so the event can be retried", cursor)
	}
}

type failRunningExecutionStore struct {
	*state.MemoryStore
}

type failingSSEWriter struct{}

func (failingSSEWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected SSE writer failure")
}

type cancelingEventStore struct {
	*state.MemoryStore
	cancel context.CancelFunc
}

func (s *cancelingEventStore) AddEvent(ctx context.Context, event events.Event) (events.Event, error) {
	s.cancel()
	return events.Event{}, ctx.Err()
}

type failingEventsSinceStore struct {
	*state.MemoryStore
}

func (*failingEventsSinceStore) EventsSince(context.Context, string, uint64) ([]events.Event, error) {
	return nil, errors.New("injected durable history failure")
}

func (s *failRunningExecutionStore) SaveExecution(ctx context.Context, execution state.Execution) error {
	if execution.Status == state.ExecutionRunning {
		return errors.New("injected running execution failure")
	}
	return s.MemoryStore.SaveExecution(ctx, execution)
}

type failCompletedExecutionStore struct {
	*state.MemoryStore
}

func (s *failCompletedExecutionStore) SaveExecution(ctx context.Context, execution state.Execution) error {
	if execution.Status == state.ExecutionCompleted {
		return errors.New("injected completed execution failure")
	}
	return s.MemoryStore.SaveExecution(ctx, execution)
}
