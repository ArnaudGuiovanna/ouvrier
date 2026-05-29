package ovr

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// TestStreamDLQOptionCompilesIntoPlan asserts the new Stream delivery options
// flow through the trigger into the runtime plan.
func TestStreamDLQOptionCompilesIntoPlan(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), StreamDLQ("kafka://tickets.dlq", 3), StreamMaxInFlight(2)),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	trigger := plans[0].Trigger
	if trigger.DLQTarget != "kafka://tickets.dlq" {
		t.Fatalf("DLQ target = %q, want kafka://tickets.dlq", trigger.DLQTarget)
	}
	if trigger.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d, want 3", trigger.MaxAttempts)
	}
	if trigger.MaxInFlight != 2 {
		t.Fatalf("max in-flight = %d, want 2", trigger.MaxInFlight)
	}
}

func TestStreamDLQOptionRejectsInvalidValues(t *testing.T) {
	if err := Validate(From(Stream("kafka://tickets"), StreamDLQ("", 3)), Sink(Log())); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("empty DLQ target error = %v, want ErrInvalidNode", err)
	}
	if err := Validate(From(Stream("kafka://tickets"), StreamDLQ("kafka://tickets.dlq", 0)), Sink(Log())); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("zero max attempts error = %v, want ErrInvalidNode", err)
	}
	if err := Validate(From(Stream("kafka://tickets"), StreamMaxInFlight(0)), Sink(Log())); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("zero max in-flight error = %v, want ErrInvalidNode", err)
	}
}

// TestRunStreamLoopRoutesToDLQAfterMaxAttempts asserts a poisoned message is
// retried up to MaxAttempts and then routed to the DLQ target with the
// stream_dead_lettered event carrying the attempt count and reason.
func TestRunStreamLoopRoutesToDLQAfterMaxAttempts(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamDLQ("kafka://tickets.dlq", 3)),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	// Always fails: every delivery is poisoned.
	prov := &alwaysFailStreamProvider{}
	// The same poison message is redelivered until it exceeds MaxAttempts.
	receiver := &redeliveringStreamReceiver{
		message: streamMessage{ID: "poison-1", Body: `{"event":"bad"}`},
		limit:   5,
	}
	dlq := &fakeStreamDLQ{}

	err = runStreamLoop(context.Background(), httpRuntime{
		provider:       prov,
		streamReceiver: receiver,
		eventStream:    stream,
		streamDLQ:      dlq,
	}, plans[0])
	if err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}

	if got := dlq.depth(); got != 1 {
		t.Fatalf("DLQ depth = %d, want 1 poisoned message routed", got)
	}
	routed := dlq.routed()[0]
	if routed.target != "kafka://tickets.dlq" {
		t.Fatalf("DLQ target = %q, want kafka://tickets.dlq", routed.target)
	}
	if routed.message.ID != "poison-1" {
		t.Fatalf("DLQ message id = %q, want poison-1", routed.message.ID)
	}
	// 3 attempts before dead-lettering.
	if prov.count() != 3 {
		t.Fatalf("provider calls = %d, want 3 attempts before DLQ", prov.count())
	}

	event, ok := findRuntimeHTTPEvent(stream.List(), events.EventStreamDeadLettered)
	if !ok {
		t.Fatalf("events = %+v, want stream dead-letter event", stream.List())
	}
	if event.Payload["dlq"] != "kafka://tickets.dlq" {
		t.Fatalf("dead-letter dlq = %v, want DLQ target", event.Payload["dlq"])
	}
	if attempt := adminNumericPayload(event.Payload, "attempt"); attempt != 3 {
		t.Fatalf("dead-letter attempt = %v, want 3", event.Payload["attempt"])
	}
	if _, ok := event.Payload["reason"]; !ok {
		t.Fatalf("dead-letter payload missing reason: %+v", event.Payload)
	}
}

// TestRunStreamLoopRedeliversBeforeMaxAttempts asserts a transient failure is
// nacked for redelivery (not dead-lettered) until it succeeds.
func TestRunStreamLoopRedeliversBeforeMaxAttempts(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamDLQ("kafka://tickets.dlq", 5)),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &failFirstStreamProvider{response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn}}
	receiver := &redeliveringStreamReceiver{
		message: streamMessage{ID: "retry-1", Body: `{"event":"created"}`},
		limit:   5,
	}
	dlq := &fakeStreamDLQ{}

	err = runStreamLoop(context.Background(), httpRuntime{
		provider:       prov,
		streamReceiver: receiver,
		eventStream:    stream,
		streamDLQ:      dlq,
	}, plans[0])
	if err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}
	if dlq.depth() != 0 {
		t.Fatalf("DLQ depth = %d, want no dead-letter (succeeded on retry)", dlq.depth())
	}
	if prov.calls != 2 {
		t.Fatalf("provider calls = %d, want one failure then success", prov.calls)
	}
	assertSinkLoggedEvent(t, stream, "output", `{"status":"stream"}`)
}

// TestReplayStreamDLQReprocessesMessages asserts ReplayStreamDLQ drains the
// DLQ back through the pipeline.
func TestReplayStreamDLQReprocessesMessages(t *testing.T) {
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), StreamDLQ("kafka://tickets.dlq", 1)),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &httpScriptedProvider{response: provider.Response{Text: `{"status":"replayed"}`, StopReason: provider.StopEndTurn}}
	dlq := newMemoryStreamDLQ()
	// Seed the DLQ as if a previous run dead-lettered the message.
	if err := dlq.Route(context.Background(), "kafka://tickets.dlq", streamMessage{ID: "poison-1", Body: `{"event":"created"}`}); err != nil {
		t.Fatalf("seed Route returned error: %v", err)
	}

	rt := httpRuntime{provider: prov, eventStream: stream, streamDLQ: dlq}
	replayed, err := rt.ReplayStreamDLQ(context.Background(), plans[0])
	if err != nil {
		t.Fatalf("ReplayStreamDLQ returned error: %v", err)
	}
	if replayed != 1 {
		t.Fatalf("replayed = %d, want 1", replayed)
	}
	if dlq.Depth("kafka://tickets.dlq") != 0 {
		t.Fatalf("DLQ depth after replay = %d, want 0", dlq.Depth("kafka://tickets.dlq"))
	}
	assertSinkLoggedEvent(t, stream, "output", `{"status":"replayed"}`)
}

// TestRunStreamLoopBoundsInFlight asserts StreamMaxInFlight applies
// backpressure so a slow handler never has more than the configured number of
// concurrent messages in flight.
func TestRunStreamLoopBoundsInFlight(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), StreamMaxInFlight(2)),
		Pipe("summarize ticket event", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &blockingStreamProvider{
		delay:    20 * time.Millisecond,
		response: provider.Response{Text: `{"status":"stream"}`, StopReason: provider.StopEndTurn},
	}
	receiver := &scriptedStreamReceiver{
		messages: []streamMessage{
			{ID: "m1", Body: `{"n":1}`},
			{ID: "m2", Body: `{"n":2}`},
			{ID: "m3", Body: `{"n":3}`},
			{ID: "m4", Body: `{"n":4}`},
		},
	}

	err = runStreamLoop(context.Background(), httpRuntime{provider: prov, streamReceiver: receiver}, plans[0])
	if err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}
	if prov.maxActive > 2 {
		t.Fatalf("max active provider calls = %d, want StreamMaxInFlight(2) to bound in-flight", prov.maxActive)
	}
	if prov.calls != 4 {
		t.Fatalf("provider calls = %d, want four stream deliveries", prov.calls)
	}
}

// streamBrokerCase enumerates a source stream URI and a matching DLQ target
// for each supported broker so the production-hardening behaviour is exercised
// at every broker boundary, not just Kafka.
type streamBrokerCase struct {
	name   string
	source string
	dlq    string
}

func streamBrokerCases() []streamBrokerCase {
	return []streamBrokerCase{
		{name: "kafka", source: "kafka://tickets", dlq: "kafka://tickets.dlq"},
		{name: "nats", source: "nats://127.0.0.1:4222/tickets.created", dlq: "nats://127.0.0.1:4222/tickets.dlq"},
		{name: "redis", source: "redis://127.0.0.1:6379/tickets", dlq: "redis://127.0.0.1:6379/tickets-dlq"},
	}
}

func TestRunStreamLoopRoutesToDLQPerBroker(t *testing.T) {
	for _, bc := range streamBrokerCases() {
		t.Run(bc.name, func(t *testing.T) {
			stream, err := events.NewEventStream()
			if err != nil {
				t.Fatalf("NewEventStream returned error: %v", err)
			}
			plans, err := compilePlans([]Node{
				From(Stream(bc.source), WorkerPool(1), StreamDLQ(bc.dlq, 2)),
				Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
				Sink(Log()),
			})
			if err != nil {
				t.Fatalf("compilePlans returned error: %v", err)
			}
			prov := &alwaysFailStreamProvider{}
			receiver := &redeliveringStreamReceiver{
				message: streamMessage{ID: "poison-1", Body: `{"event":"bad"}`},
				limit:   5,
			}
			dlq := &fakeStreamDLQ{}

			err = runStreamLoop(context.Background(), httpRuntime{
				provider:       prov,
				streamReceiver: receiver,
				eventStream:    stream,
				streamDLQ:      dlq,
			}, plans[0])
			if err != nil {
				t.Fatalf("runStreamLoop returned error: %v", err)
			}
			if dlq.depth() != 1 {
				t.Fatalf("DLQ depth = %d, want 1", dlq.depth())
			}
			if got := dlq.routed()[0].target; got != bc.dlq {
				t.Fatalf("DLQ target = %q, want %q", got, bc.dlq)
			}
			if prov.count() != 2 {
				t.Fatalf("provider calls = %d, want 2 attempts before DLQ", prov.count())
			}
		})
	}
}

func TestRunStreamLoopReplayPerBroker(t *testing.T) {
	for _, bc := range streamBrokerCases() {
		t.Run(bc.name, func(t *testing.T) {
			stream, err := events.NewEventStream()
			if err != nil {
				t.Fatalf("NewEventStream returned error: %v", err)
			}
			plans, err := compilePlans([]Node{
				From(Stream(bc.source), StreamDLQ(bc.dlq, 1)),
				Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
				Sink(Log()),
			})
			if err != nil {
				t.Fatalf("compilePlans returned error: %v", err)
			}
			prov := &httpScriptedProvider{response: provider.Response{Text: `{"status":"replayed"}`, StopReason: provider.StopEndTurn}}
			dlq := newMemoryStreamDLQ()
			if err := dlq.Route(context.Background(), bc.dlq, streamMessage{ID: "poison-1", Body: `{"event":"created"}`}); err != nil {
				t.Fatalf("seed Route returned error: %v", err)
			}
			rt := httpRuntime{provider: prov, eventStream: stream, streamDLQ: dlq}
			replayed, err := rt.ReplayStreamDLQ(context.Background(), plans[0])
			if err != nil {
				t.Fatalf("ReplayStreamDLQ returned error: %v", err)
			}
			if replayed != 1 {
				t.Fatalf("replayed = %d, want 1", replayed)
			}
			if dlq.Depth(bc.dlq) != 0 {
				t.Fatalf("DLQ depth after replay = %d, want 0", dlq.Depth(bc.dlq))
			}
		})
	}
}

func TestRunStreamLoopBackpressurePerBroker(t *testing.T) {
	for _, bc := range streamBrokerCases() {
		t.Run(bc.name, func(t *testing.T) {
			plans, err := compilePlans([]Node{
				From(Stream(bc.source), StreamMaxInFlight(2)),
				Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
				Sink(Log()),
			})
			if err != nil {
				t.Fatalf("compilePlans returned error: %v", err)
			}
			prov := &blockingStreamProvider{
				delay:    15 * time.Millisecond,
				response: provider.Response{Text: `{"status":"ok"}`, StopReason: provider.StopEndTurn},
			}
			receiver := &scriptedStreamReceiver{messages: []streamMessage{
				{ID: "m1", Body: `{"n":1}`},
				{ID: "m2", Body: `{"n":2}`},
				{ID: "m3", Body: `{"n":3}`},
				{ID: "m4", Body: `{"n":4}`},
				{ID: "m5", Body: `{"n":5}`},
			}}
			err = runStreamLoop(context.Background(), httpRuntime{provider: prov, streamReceiver: receiver}, plans[0])
			if err != nil {
				t.Fatalf("runStreamLoop returned error: %v", err)
			}
			if prov.maxActive > 2 {
				t.Fatalf("max active = %d, want bounded in-flight of 2", prov.maxActive)
			}
		})
	}
}

func TestSummarizeRuntimeMetricsCountsStreamDLQAndRedeliveries(t *testing.T) {
	recorded := []events.Event{
		{Kind: events.EventStreamRedelivered, Payload: map[string]any{"attempt": 1}},
		{Kind: events.EventStreamRedelivered, Payload: map[string]any{"attempt": 2}},
		{Kind: events.EventStreamDeadLettered, Payload: map[string]any{"attempt": 3, "dlq": "kafka://tickets.dlq"}},
	}
	rendered := summarizeRuntimeMetrics(recorded).render()
	for _, want := range []string{
		"ouvrier_stream_dead_lettered_total 1",
		"ouvrier_stream_redelivered_total 2",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("metrics missing %q\n---\n%s", want, rendered)
		}
	}
}

type alwaysFailStreamProvider struct {
	calls atomic.Int64
}

func (p *alwaysFailStreamProvider) Name() string { return "always-fail" }

func (p *alwaysFailStreamProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.calls.Add(1)
	return provider.Response{}, provider.PermanentError(errors.New("poison message"))
}

func (p *alwaysFailStreamProvider) count() int { return int(p.calls.Load()) }

// redeliveringStreamReceiver simulates a broker: it delivers a message, and
// only redelivers it after it has been nacked. Once acked, the message is gone
// and the stream ends. Receive blocks until the message is available again (or
// the message has been acked) so it composes correctly with the worker-pool
// backpressure in runStreamLoop. A hard limit guards against infinite loops.
type redeliveringStreamReceiver struct {
	mu        sync.Mutex
	cond      *sync.Cond
	message   streamMessage
	limit     int
	delivered int
	available bool
	settled   bool // acked or fully exhausted: no more deliveries
	started   bool
}

func (r *redeliveringStreamReceiver) Receive(ctx context.Context, uri string) (streamMessage, error) {
	if err := ctx.Err(); err != nil {
		return streamMessage{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cond == nil {
		r.cond = sync.NewCond(&r.mu)
	}
	if !r.started {
		r.started = true
		r.available = true
	}
	for !r.available && !r.settled {
		r.cond.Wait()
	}
	if r.settled || r.delivered >= r.limit {
		return streamMessage{}, io.EOF
	}
	r.delivered++
	r.available = false
	msg := r.message
	msg.nack = func(context.Context, error) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.available = true
		r.cond.Broadcast()
		return nil
	}
	msg.ack = func(context.Context) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.settled = true
		r.cond.Broadcast()
		return nil
	}
	return msg, nil
}

type routedDLQMessage struct {
	target  string
	message streamMessage
}

type fakeStreamDLQ struct {
	mu       sync.Mutex
	messages []routedDLQMessage
}

func (d *fakeStreamDLQ) Route(ctx context.Context, target string, message streamMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = append(d.messages, routedDLQMessage{target: target, message: message})
	return nil
}

func (d *fakeStreamDLQ) Drain(ctx context.Context, target string) ([]streamMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []streamMessage
	for _, m := range d.messages {
		if m.target == target {
			out = append(out, m.message)
		}
	}
	d.messages = nil
	return out, nil
}

func (d *fakeStreamDLQ) Depth(target string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, m := range d.messages {
		if m.target == target {
			n++
		}
	}
	return n
}

func (d *fakeStreamDLQ) depth() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.messages)
}

func (d *fakeStreamDLQ) routed() []routedDLQMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]routedDLQMessage(nil), d.messages...)
}
