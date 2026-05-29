package ovr

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// TestStreamAckPolicyCompilesIntoPlan asserts the StreamAckPolicy option flows
// through the trigger into the runtime plan.
func TestStreamAckPolicyCompilesIntoPlan(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), StreamAckPolicy(StreamAckManual)),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if got := plans[0].Trigger.AckPolicy; got != string(StreamAckManual) {
		t.Fatalf("ack policy = %q, want %q", got, StreamAckManual)
	}
}

func TestStreamAckPolicyDefaultsToAuto(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	if got := plans[0].Trigger.AckPolicy; got != "" && got != string(StreamAckAuto) {
		t.Fatalf("default ack policy = %q, want empty/auto", got)
	}
}

func TestStreamAckPolicyRejectsUnknownMode(t *testing.T) {
	if err := Validate(From(Stream("kafka://tickets"), StreamAckPolicy("sometimes")), Sink(Log())); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("unknown ack policy error = %v, want ErrInvalidNode", err)
	}
}

// TestStreamAckPolicyManualSkipsAutoAck asserts that under the manual ack
// policy a successfully processed message is NOT auto-acked by the runtime: the
// broker redelivers until the source's own ack is invoked. With manual acking a
// message lacking an explicit ack is left for the broker.
func TestStreamAckPolicyManualSkipsAutoAck(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamAckPolicy(StreamAckManual)),
		Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &httpScriptedProvider{response: provider.Response{Text: `{"status":"ok"}`, StopReason: provider.StopEndTurn}}
	acked := 0
	receiver := &scriptedAckReceiver{
		message: streamMessage{ID: "m1", Body: `{"n":1}`},
		onAck:   func() { acked++ },
	}
	stream, _ := events.NewEventStream()
	rt := httpRuntime{provider: prov, streamReceiver: receiver, eventStream: stream}
	if err := runStreamLoop(context.Background(), rt, plans[0]); err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}
	if acked != 0 {
		t.Fatalf("auto-ack count = %d, want 0 under manual ack policy", acked)
	}
}

// TestStreamAckPolicyAutoAcksOnSuccess asserts the default (auto) policy acks a
// processed message so the broker stops redelivering it.
func TestStreamAckPolicyAutoAcksOnSuccess(t *testing.T) {
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamAckPolicy(StreamAckAuto)),
		Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &httpScriptedProvider{response: provider.Response{Text: `{"status":"ok"}`, StopReason: provider.StopEndTurn}}
	acked := 0
	receiver := &scriptedAckReceiver{
		message: streamMessage{ID: "m1", Body: `{"n":1}`},
		onAck:   func() { acked++ },
	}
	stream, _ := events.NewEventStream()
	rt := httpRuntime{provider: prov, streamReceiver: receiver, eventStream: stream}
	if err := runStreamLoop(context.Background(), rt, plans[0]); err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}
	if acked != 1 {
		t.Fatalf("auto-ack count = %d, want 1 under auto ack policy", acked)
	}
}

// scriptedAckReceiver delivers a single message once then reports EOF, invoking
// onAck whenever the runtime acks the delivery.
type scriptedAckReceiver struct {
	message   streamMessage
	onAck     func()
	delivered bool
}

func (r *scriptedAckReceiver) Receive(ctx context.Context, _ string) (streamMessage, error) {
	if err := ctx.Err(); err != nil {
		return streamMessage{}, err
	}
	if r.delivered {
		return streamMessage{}, io.EOF
	}
	r.delivered = true
	msg := r.message
	msg.ack = func(context.Context) error {
		if r.onAck != nil {
			r.onAck()
		}
		return nil
	}
	return msg, nil
}
