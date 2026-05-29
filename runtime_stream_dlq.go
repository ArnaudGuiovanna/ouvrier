package ovr

import (
	"context"
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
