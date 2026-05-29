package ovr

import (
	"context"
	"net/url"
	"strings"
	"sync"

	runtimeplan "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

// streamDLQ routes poisoned stream messages (those that exceeded the
// configured max attempts) to a dead-letter target and supports draining the
// target for replay. Implementations are injectable so brokers and tests can
// provide their own transport while the runtime stays transport-agnostic.
type streamDLQ interface {
	// Route stores a poisoned message under the dead-letter target.
	Route(ctx context.Context, target string, message streamMessage) error
	// Drain removes and returns every message currently held for target.
	Drain(ctx context.Context, target string) ([]streamMessage, error)
}

// memoryStreamDLQ is the default in-process dead-letter queue. It buffers
// poisoned messages per target so they can be inspected, counted, and replayed
// without depending on an external broker. Message bodies are held verbatim so
// replay reproduces the original delivery; nothing here is persisted to the
// event/state stores, keeping secrets and bodies out of durable logs.
type memoryStreamDLQ struct {
	mu       sync.Mutex
	messages map[string][]streamMessage
}

func newMemoryStreamDLQ() *memoryStreamDLQ {
	return &memoryStreamDLQ{messages: make(map[string][]streamMessage)}
}

func (d *memoryStreamDLQ) Route(_ context.Context, target string, message streamMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.messages == nil {
		d.messages = make(map[string][]streamMessage)
	}
	// Drop the broker ack/nack closures: a dead-lettered message is no longer
	// owned by the source broker, and replay re-runs the pipeline fresh.
	message.ack = nil
	message.nack = nil
	d.messages[target] = append(d.messages[target], message)
	return nil
}

func (d *memoryStreamDLQ) Drain(_ context.Context, target string) ([]streamMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	messages := d.messages[target]
	delete(d.messages, target)
	return messages, nil
}

// Depth reports the number of messages currently held for target. It is used
// to expose DLQ depth without mutating the queue.
func (d *memoryStreamDLQ) Depth(target string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.messages[target])
}

// streamDLQPublisher publishes a dead-lettered message body to a broker target.
// It is a package variable so tests can observe routing without a live broker;
// production routes through the same publishQueue machinery used by the queue
// push terminals (kafka://, nats://, redis://, sqs://, http(s)://).
var streamDLQPublisher = publishQueue

// routingStreamDLQ is the default production dead-letter queue. It publishes the
// poisoned message body to the configured broker target on the real transport
// (reusing publishQueue) and also retains a copy in an in-process buffer so the
// DLQ remains drainable for replay and inspectable for depth. Targets with an
// empty or in-process scheme (mem://) are kept memory-only.
//
// Only the message body is published to the broker; ack/nack closures and the
// source delivery are never forwarded. DLQ targets are credential-stripped via
// streamDisplayURI anywhere they surface in events/logs.
type routingStreamDLQ struct {
	memory  *memoryStreamDLQ
	publish func(ctx context.Context, rawURI, output string) error
}

func newRoutingStreamDLQ() *routingStreamDLQ {
	return &routingStreamDLQ{
		memory:  newMemoryStreamDLQ(),
		publish: streamDLQPublisher,
	}
}

// brokerStreamDLQScheme reports whether target names a broker transport that the
// DLQ can publish to over the wire (as opposed to the in-process mem:// scheme).
func brokerStreamDLQScheme(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "kafka", "nats", "redis", "rediss", "sqs", "http", "https":
		return true
	default:
		return false
	}
}

func (d *routingStreamDLQ) Route(ctx context.Context, target string, message streamMessage) error {
	if brokerStreamDLQScheme(target) {
		publish := d.publish
		if publish == nil {
			publish = streamDLQPublisher
		}
		if err := publish(ctx, target, message.Body); err != nil {
			return err
		}
	}
	// Retain a copy so the DLQ stays drainable for replay and depth, mirroring
	// the in-process default. Broker-backed targets are also buffered because
	// their wire protocols (Kafka topics, NATS subjects, Redis streams) cannot
	// be drained for replay without a dedicated consumer.
	return d.memory.Route(ctx, target, message)
}

func (d *routingStreamDLQ) Drain(ctx context.Context, target string) ([]streamMessage, error) {
	return d.memory.Drain(ctx, target)
}

func (d *routingStreamDLQ) Depth(target string) int {
	return d.memory.Depth(target)
}

// ReplayStreamDLQ drains the dead-letter target configured on the plan and
// reprocesses each message through the pipeline. It returns the number of
// messages successfully reprocessed. A reprocess failure stops the replay and
// returns the error along with the count reprocessed so far so the caller can
// retry the remainder later.
func (rt httpRuntime) ReplayStreamDLQ(ctx context.Context, plan runtimeplan.Plan) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rt.streamDLQ == nil {
		return 0, nil
	}
	target := streamDLQTarget(plan)
	if target == "" {
		return 0, nil
	}
	messages, err := rt.streamDLQ.Drain(ctx, target)
	if err != nil {
		return 0, err
	}
	replayed := 0
	for _, message := range messages {
		if _, err := runStreamPlanOnce(ctx, rt, plan, message); err != nil {
			// Re-dead-letter the message we could not reprocess so it is not
			// lost, then return the count reprocessed so far.
			_ = rt.streamDLQ.Route(ctx, target, message)
			return replayed, err
		}
		replayed++
	}
	return replayed, nil
}

func streamDLQTarget(plan runtimeplan.Plan) string {
	return plan.Trigger.DLQTarget
}

// streamAttemptTracker counts processing attempts per message ID so a message
// redelivered by the broker accrues attempts across deliveries until it
// succeeds or exhausts its retry budget. Messages without an ID cannot be
// correlated across redeliveries, so each delivery counts as attempt one.
type streamAttemptTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

func newStreamAttemptTracker() *streamAttemptTracker {
	return &streamAttemptTracker{counts: make(map[string]int)}
}

func (t *streamAttemptTracker) next(id string) int {
	if t == nil {
		return 1
	}
	if id == "" {
		return 1
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts == nil {
		t.counts = make(map[string]int)
	}
	t.counts[id]++
	return t.counts[id]
}

func (t *streamAttemptTracker) clear(id string) {
	if t == nil || id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, id)
}
