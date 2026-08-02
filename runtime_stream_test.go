package ovr

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

func TestValidateStreamTriggerURIs(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		wantErr bool
	}{
		{name: "kafka topic host", uri: "kafka://tickets"},
		{name: "nats subject path", uri: "nats://127.0.0.1:4222/tickets.created"},
		{name: "redis stream path", uri: "redis://127.0.0.1:6379/tickets"},
		{name: "unsupported scheme", uri: "mqtt://tickets", wantErr: true},
		{name: "missing redis stream", uri: "redis://127.0.0.1:6379", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(
				From(Stream(tt.uri)),
				Sink(Log()),
			)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidNode) {
					t.Fatalf("Validate error = %v, want ErrInvalidNode", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate returned error: %v", err)
			}
		})
	}
}

func TestRunStreamPlanOnceRunsPipelineAndLogsOutput(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn},
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	result, err := runStreamPlanOnce(context.Background(), httpRuntime{provider: scripted, eventStream: stream}, plans[0], streamMessage{
		ID:   "msg-1",
		Body: `{"event":"created"}`,
		Metadata: map[string]string{
			"partition": "7",
		},
	})
	if err != nil {
		t.Fatalf("runStreamPlanOnce returned error: %v", err)
	}

	if result.Output != `{"status":"stream"}` {
		t.Fatalf("output = %q, want stream provider output", result.Output)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	input := decodeProviderInput(t, scripted.requests[0].Messages[0].Text())
	assertRawJSONField(t, input, "trigger", `"stream"`)
	assertRawJSONField(t, input, "uri", `"kafka://tickets"`)
	assertRawJSONField(t, input, "id", `"msg-1"`)
	assertRawJSONField(t, input, "body", `{"event":"created"}`)
	assertRawJSONField(t, input, "metadata", `{"partition":"7"}`)
	assertSinkLoggedEvent(t, stream, "output", `{"status":"stream"}`)
}

func TestRunStreamPlanOnceSinksDirectInput(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("nats://127.0.0.1:4222/tickets.created")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	_, err = runStreamPlanOnce(context.Background(), httpRuntime{eventStream: stream}, plans[0], streamMessage{
		ID:   "nats-1",
		Body: `plain-text`,
	})
	if err != nil {
		t.Fatalf("runStreamPlanOnce returned error: %v", err)
	}

	event := assertSinkLoggedEvent(t, stream, "input", `{"body":"plain-text","id":"nats-1","trigger":"stream","uri":"nats://127.0.0.1:4222/tickets.created"}`)
	if event.ExecID != "" {
		t.Fatalf("direct stream sink ExecID = %q, want empty without idempotency session", event.ExecID)
	}
}

func TestRunStreamPlanOnceSkipsDuplicateDeliveryWhenStateStoreConfigured(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	delivery := streamMessage{ID: "delivery-1", Body: `{"event":"created"}`}
	rt := httpRuntime{stateStore: store, eventStream: stream}

	if _, err := runStreamPlanOnce(context.Background(), rt, plans[0], delivery); err != nil {
		t.Fatalf("first runStreamPlanOnce returned error: %v", err)
	}
	if _, err := runStreamPlanOnce(context.Background(), rt, plans[0], delivery); err != nil {
		t.Fatalf("second runStreamPlanOnce returned error: %v", err)
	}

	sinkEvents := 0
	idempotencyEvents := 0
	duplicateDecisions := 0
	for _, event := range stream.List() {
		switch event.Kind {
		case events.EventSinkLogged:
			sinkEvents++
		case events.EventIdempotencyDecision:
			idempotencyEvents++
			if event.Payload["decision"] == "duplicate" {
				duplicateDecisions++
			}
			if _, leaked := event.Payload["key"]; leaked {
				t.Fatalf("idempotency event leaked key payload: %+v", event.Payload)
			}
		}
	}
	if sinkEvents != 1 {
		t.Fatalf("sink events = %d, want one side effect", sinkEvents)
	}
	if idempotencyEvents != 2 || duplicateDecisions != 1 {
		t.Fatalf("idempotency events=%d duplicate=%d, want reserved and duplicate decisions", idempotencyEvents, duplicateDecisions)
	}
}

func TestConcurrentStreamDeliveryParksPendingReservationWithoutExecutingTwice(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Pipe("summarize ticket event", Model("test/stream-concurrency")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	provider := newGatedStreamProvider()
	rt := httpRuntime{provider: provider, stateStore: store, eventStream: stream}
	delivery := streamMessage{ID: "delivery-concurrent", Body: `{"event":"created"}`}

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runStreamPlanOnce(context.Background(), rt, plans[0], delivery)
		firstDone <- runErr
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not reach the provider")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, pendingErr := runStreamPlanOnce(ctx, rt, plans[0], delivery)
	cancel()
	if !errors.Is(pendingErr, errStreamIdempotencyPending) {
		t.Fatalf("concurrent delivery error = %v, want pending reservation", pendingErr)
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("provider calls while reservation pending = %d, want exactly one", calls)
	}

	nacked, acked := false, false
	parked := delivery
	parked.ack = func(context.Context) error {
		acked = true
		return nil
	}
	parked.nack = func(_ context.Context, deliveryErr error) error {
		nacked = errors.Is(deliveryErr, errStreamIdempotencyPending)
		return nil
	}
	if err := rt.processStreamMessage(context.Background(), plans[0], parked, newStreamAttemptTracker()); err != nil {
		t.Fatalf("process pending stream delivery returned error: %v", err)
	}
	if !nacked || acked {
		t.Fatalf("pending delivery acked=%v nacked=%v, want parked with nack only", acked, nacked)
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("provider calls after parked redelivery = %d, want exactly one", calls)
	}

	close(provider.release)
	select {
	case runErr := <-firstDone:
		if runErr != nil {
			t.Fatalf("first delivery returned error: %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("first delivery did not finish after provider release")
	}
	if _, err := runStreamPlanOnce(context.Background(), rt, plans[0], delivery); err != nil {
		t.Fatalf("completed duplicate returned error: %v", err)
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("provider calls after completed duplicate = %d, want exactly one", calls)
	}
	if _, ok := findStreamIdempotencyDecision(stream.List(), "in_progress"); !ok {
		t.Fatalf("events = %+v, want observable in_progress decision", stream.List())
	}
}

func TestStreamReservationSetupFailureBecomesRetryable(t *testing.T) {
	base := state.NewMemoryStore()
	failing := streamFailSaveSessionStore{Store: base, IdempotencyOutcomeStore: base}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	delivery := streamMessage{ID: "delivery-setup-failure", Body: `{"event":"created"}`}

	if _, err := runStreamPlanOnce(context.Background(), httpRuntime{stateStore: failing}, plans[0], delivery); err == nil {
		t.Fatal("runStreamPlanOnce returned nil, want injected SaveSession failure")
	}
	key := streamIdempotencyReservationKey(plans[0], delivery.ID)
	record, found, err := base.Idempotency(context.Background(), key)
	if err != nil || !found || record.Outcome != state.IdempotencyFailed {
		t.Fatalf("reservation after setup failure = %+v found=%v err=%v, want failed", record, found, err)
	}
	if _, err := runStreamPlanOnce(context.Background(), httpRuntime{stateStore: base}, plans[0], delivery); err != nil {
		t.Fatalf("retry after setup failure returned error: %v", err)
	}
}

func TestRunStreamPlanOnceRedactsStreamURICredentialsFromInputsAndEvents(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("redis://user:secret@127.0.0.1:6379/tickets")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}

	_, err = runStreamPlanOnce(context.Background(), httpRuntime{stateStore: store, eventStream: stream}, plans[0], streamMessage{
		ID:   "delivery-1",
		Body: `{"event":"created"}`,
	})
	if err != nil {
		t.Fatalf("runStreamPlanOnce returned error: %v", err)
	}

	for _, event := range stream.List() {
		for _, value := range event.Payload {
			if strings.Contains(strings.TrimSpace(toStreamTestString(value)), "secret") {
				t.Fatalf("event payload leaked stream URI credentials: %+v", event.Payload)
			}
		}
	}
}

func TestRunStreamLoopBoundsWorkerPool(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1)),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	provider := &blockingStreamProvider{
		delay:    20 * time.Millisecond,
		response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn},
	}
	receiver := &scriptedStreamReceiver{
		messages: []streamMessage{
			{ID: "msg-1", Body: `{"event":"one"}`},
			{ID: "msg-2", Body: `{"event":"two"}`},
		},
	}

	err = runStreamLoop(context.Background(), httpRuntime{
		provider:       provider,
		streamReceiver: receiver,
	}, plans[0])
	if err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}
	if provider.maxActive != 1 {
		t.Fatalf("max active provider calls = %d, want WorkerPool(1) to keep one active call", provider.maxActive)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want two stream deliveries", provider.calls)
	}
}

func TestRunStreamPlansReturnsNilOnContextCancellation(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	receiver := &blockingUntilCanceledStreamReceiver{started: make(chan struct{})}
	errCh := make(chan error, 1)

	go func() {
		errCh <- runStreamPlans(ctx, httpRuntime{streamReceiver: receiver}, plans)
	}()
	select {
	case <-receiver.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("stream receiver did not start")
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runStreamPlans returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runStreamPlans did not stop after context cancellation")
	}
}

func TestServeStreamPlansMountsAdminEndpointsWhileLoopRuns(t *testing.T) {
	t.Setenv("OUVRIER_ENV", "dev")
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := localRuntimeAddr(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- serveStreamPlansWithContext(ctx, addr, httpRuntime{
			streamReceiver: &blockingUntilCanceledStreamReceiver{started: make(chan struct{})},
		}, plans)
	}()

	waitAdminHealth(t, addr)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveStreamPlansWithContext returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveStreamPlansWithContext did not stop after cancellation")
	}
}

func TestRunStreamLoopDeadLettersFailedDeliveryAndContinues(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1)),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	provider := &failFirstStreamProvider{
		response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn},
	}
	receiver := &scriptedStreamReceiver{
		messages: []streamMessage{
			{ID: "msg-1", Body: `{"event":"one"}`},
			{ID: "msg-2", Body: `{"event":"two"}`},
		},
	}

	err = runStreamLoop(context.Background(), httpRuntime{
		provider:       provider,
		streamReceiver: receiver,
		eventStream:    stream,
	}, plans[0])
	if err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want failed delivery plus next delivery", provider.calls)
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventStreamDeadLettered); !ok {
		t.Fatalf("events = %+v, want stream dead-letter event", stream.List())
	}
	event, _ := findRuntimeHTTPEvent(stream.List(), events.EventStreamDeadLettered)
	if event.Payload["dlq"] != "event_only" {
		t.Fatalf("dead-letter payload = %+v, want event-only DLQ marker", event.Payload)
	}
	assertSinkLoggedEvent(t, stream, "output", `{"status":"stream"}`)
}

func TestRunStreamPlanOnceRetriesFailedIdempotencyReservation(t *testing.T) {
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("redis://127.0.0.1:6379/tickets")),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	provider := &failFirstStreamProvider{
		response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn},
	}
	rt := httpRuntime{provider: provider, stateStore: store, eventStream: stream}
	delivery := streamMessage{ID: "1747840000000-0", Body: `{"event":"created"}`}

	if _, err := runStreamPlanOnce(context.Background(), rt, plans[0], delivery); err == nil {
		t.Fatal("first runStreamPlanOnce returned nil, want failure")
	}
	if _, err := runStreamPlanOnce(context.Background(), rt, plans[0], delivery); err != nil {
		t.Fatalf("second runStreamPlanOnce returned error: %v", err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want retry after failed execution", provider.calls)
	}
	assertSinkLoggedEvent(t, stream, "output", `{"status":"stream"}`)
	if _, ok := findStreamIdempotencyDecision(stream.List(), "retry"); !ok {
		t.Fatalf("events = %+v, want retry idempotency decision", stream.List())
	}
}

func TestProcessStreamMessageAcknowledgesSuccessfulDelivery(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("nats://127.0.0.1:4222/tickets.created")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	acked := false

	err = httpRuntime{eventStream: stream}.processStreamMessage(context.Background(), plans[0], streamMessage{
		ID:   "msg-1",
		Body: `{"event":"created"}`,
		ack: func(ctx context.Context) error {
			acked = true
			return ctx.Err()
		},
	}, newStreamAttemptTracker())
	if err != nil {
		t.Fatalf("processStreamMessage returned error: %v", err)
	}
	if !acked {
		t.Fatal("stream delivery was not acknowledged after successful processing")
	}
}

func TestProcessStreamMessageDeadLettersAckFailure(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("nats://127.0.0.1:4222/tickets.created")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	ackErr := errors.New("ack failed")

	err = httpRuntime{eventStream: stream}.processStreamMessage(context.Background(), plans[0], streamMessage{
		ID:   "msg-1",
		Body: `{"event":"created"}`,
		ack: func(ctx context.Context) error {
			return ackErr
		},
	}, newStreamAttemptTracker())
	if err != nil {
		t.Fatalf("processStreamMessage returned error: %v", err)
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventStreamDeadLettered); !ok {
		t.Fatalf("events = %+v, want stream dead-letter event for ack failure", stream.List())
	}
}

func toStreamTestString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func findStreamIdempotencyDecision(recorded []events.Event, decision string) (events.Event, bool) {
	for _, event := range recorded {
		if event.Kind == events.EventIdempotencyDecision && event.Payload["decision"] == decision {
			return event, true
		}
	}
	return events.Event{}, false
}

type failFirstStreamProvider struct {
	calls    int
	response provider.Response
}

func (p *failFirstStreamProvider) Name() string {
	return "fail-first"
}

func (p *failFirstStreamProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.calls++
	if p.calls == 1 {
		return provider.Response{}, provider.PermanentError(errors.New("stream delivery failed"))
	}
	return p.response, nil
}

type scriptedStreamReceiver struct {
	mu       sync.Mutex
	messages []streamMessage
	index    int
}

func (r *scriptedStreamReceiver) Receive(ctx context.Context, uri string) (streamMessage, error) {
	if err := ctx.Err(); err != nil {
		return streamMessage{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.index >= len(r.messages) {
		return streamMessage{}, io.EOF
	}
	message := r.messages[r.index]
	r.index++
	return message, nil
}

type blockingUntilCanceledStreamReceiver struct {
	started chan struct{}
	once    sync.Once
}

func (r *blockingUntilCanceledStreamReceiver) Receive(ctx context.Context, uri string) (streamMessage, error) {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	return streamMessage{}, ctx.Err()
}

type blockingStreamProvider struct {
	mu        sync.Mutex
	calls     int
	active    int
	maxActive int
	delay     time.Duration
	response  provider.Response
}

type gatedStreamProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func newGatedStreamProvider() *gatedStreamProvider {
	return &gatedStreamProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *gatedStreamProvider) Name() string { return "gated-stream" }

func (p *gatedStreamProvider) Complete(ctx context.Context, _ provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
	}
	return provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn}, nil
}

func (p *gatedStreamProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type streamFailSaveSessionStore struct {
	state.Store
	state.IdempotencyOutcomeStore
}

func (streamFailSaveSessionStore) SaveSession(context.Context, runtimeplan.Session) error {
	return errors.New("injected SaveSession failure")
}

func (p *blockingStreamProvider) Name() string {
	return "blocking"
}

func (p *blockingStreamProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	p.calls++
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()

	timer := time.NewTimer(p.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return provider.Response{}, ctx.Err()
	}

	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return p.response, nil
}
