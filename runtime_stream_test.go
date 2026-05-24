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
	t.Setenv("PIP_ENV", "dev")
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
	})
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
	})
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
