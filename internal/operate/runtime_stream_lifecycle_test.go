package operate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// abandonedStreamModel fills RunTurn's public event buffer and then enters one
// more delta callback. A frontend which stopped receiving leaves that callback
// blocked until the runtime activity context is cancelled.
type abandonedStreamModel struct {
	bufferFilled chan struct{}
	emitReleased chan struct{}
}

func newAbandonedStreamModel() *abandonedStreamModel {
	return &abandonedStreamModel{
		bufferFilled: make(chan struct{}),
		emitReleased: make(chan struct{}),
	}
}

func (m *abandonedStreamModel) Complete(_ context.Context, _ provider.Request, onDelta func(string)) (provider.Response, error) {
	// StreamUser occupies one of the 32 channel slots. Each 300-byte provider
	// chunk clears the redactor look-behind and therefore emits one delta.
	for range 31 {
		onDelta(strings.Repeat("x", 300))
	}
	close(m.bufferFilled)
	onDelta(strings.Repeat("y", 300)) // blocks while the consumer is absent
	close(m.emitReleased)
	return provider.Response{Text: "done"}, nil
}

func TestRunTurnAbandonedConsumerDoesNotBlockInterrupt(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := newAbandonedStreamModel()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/abandoned-stream"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	streamCtx, cancelStream := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelStream()
		_ = runtime.Close()
	})
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stream, err := runtime.RunTurn(streamCtx, started.Session.ID, "inspect the worker", "prompt")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	_ = stream // Deliberately abandon the event consumer.
	waitForAbandonedStreamBuffer(t, model.bufferFilled)

	interruptDone := make(chan error, 1)
	go func() {
		_, interruptErr := runtime.Interrupt(context.Background(), started.Session.ID, "stop abandoned stream")
		interruptDone <- interruptErr
	}()
	select {
	case interruptErr := <-interruptDone:
		if interruptErr != nil {
			t.Fatalf("Interrupt() error = %v", interruptErr)
		}
	case <-time.After(2 * time.Second):
		cancelStream()
		t.Fatal("Interrupt remained blocked behind an abandoned full event stream")
	}
	select {
	case <-model.emitReleased:
	case <-time.After(time.Second):
		t.Fatal("runtime cancellation did not release the blocked stream emitter")
	}
}

func TestRunTurnAbandonedConsumerDoesNotBlockClose(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := newAbandonedStreamModel()
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/abandoned-stream-close"})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	streamCtx, cancelStream := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelStream()
		_ = runtime.Close()
	})
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stream, err := runtime.RunTurn(streamCtx, started.Session.ID, "inspect the worker", "prompt")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	_ = stream // Deliberately abandon the event consumer.
	waitForAbandonedStreamBuffer(t, model.bufferFilled)

	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	select {
	case closeErr := <-closeDone:
		if closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	case <-time.After(2 * time.Second):
		cancelStream()
		t.Fatal("Close remained blocked behind an abandoned full event stream")
	}
	select {
	case <-model.emitReleased:
	case <-time.After(time.Second):
		t.Fatal("runtime shutdown did not release the blocked stream emitter")
	}
}

func waitForAbandonedStreamBuffer(t *testing.T, filled <-chan struct{}) {
	t.Helper()
	select {
	case <-filled:
	case <-time.After(time.Second):
		t.Fatal("model did not fill the stream buffer")
	}
}
