package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestOperateRPCBoundsQueuedRequests(t *testing.T) {
	dir := t.TempDir()
	model := &boundedRPCModel{started: make(chan struct{})}
	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{
		Dir: dir, Driver: operate.ManualDriver{}, Model: model, ModelID: "test/rpc-capacity",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	requestCount := operateRPCWorkerLimit + operateRPCQueueLimit + 20
	var input strings.Builder
	for i := range requestCount {
		input.WriteString(`{"id":"request-`)
		input.WriteString(strconv.Itoa(i))
		input.WriteString(`","type":"prompt","text":"inspect the worker"}` + "\n")
	}

	var output bytes.Buffer
	app := New("dev", WithStreams(strings.NewReader(input.String()), &output, io.Discard))
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	err = app.runOperateRPC(ctx, runtime, operateConfig{Dir: dir, Agent: "manual"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runOperateRPC() error = %v, want caller deadline after bounded workers settle", err)
	}
	overloaded := strings.Count(output.String(), `"error":"operate rpc overloaded:`)
	if overloaded < requestCount-(operateRPCWorkerLimit+operateRPCQueueLimit) {
		t.Fatalf("overload responses = %d, want at least %d; output=%s", overloaded, requestCount-(operateRPCWorkerLimit+operateRPCQueueLimit), output.String())
	}
}

func TestOperateRPCEncodeFailureReturnsWhileInputReadIsBlocked(t *testing.T) {
	dir := t.TempDir()
	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: dir, Driver: operate.ManualDriver{}})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	wantErr := errors.New("rpc output closed")
	reader := newBlockingAfterRPCLine(`{"id":"one","type":"prompt","text":"/help"}` + "\n")
	t.Cleanup(reader.releaseRead)
	app := New("dev", WithStreams(reader, failingRPCWriter{err: wantErr}, io.Discard))
	done := make(chan error, 1)
	go func() {
		done <- app.runOperateRPC(context.Background(), runtime, operateConfig{Dir: dir, Agent: "manual"})
	}()

	select {
	case <-reader.blocked:
	case <-time.After(time.Second):
		t.Fatal("RPC scanner did not block on the still-open input")
	}
	select {
	case runErr := <-done:
		if !errors.Is(runErr, wantErr) {
			t.Fatalf("runOperateRPC() error = %v, want encoding failure", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("encoding failure did not terminate RPC while stdin stayed open")
	}
	if got := reader.closeCalls.Load(); got != 0 {
		t.Fatalf("RPC closed caller-owned input %d time(s)", got)
	}
	reader.releaseRead()
}

type boundedRPCModel struct {
	started chan struct{}
	once    sync.Once
}

func (m *boundedRPCModel) Complete(ctx context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	return provider.Response{}, ctx.Err()
}

type failingRPCWriter struct{ err error }

func (w failingRPCWriter) Write([]byte) (int, error) { return 0, w.err }

type blockingAfterRPCLine struct {
	line        []byte
	offset      int
	blocked     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
	releaseOnce sync.Once
	closeCalls  atomic.Int32
}

func newBlockingAfterRPCLine(line string) *blockingAfterRPCLine {
	return &blockingAfterRPCLine{line: []byte(line), blocked: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingAfterRPCLine) Read(destination []byte) (int, error) {
	if r.offset < len(r.line) {
		n := copy(destination, r.line[r.offset:])
		r.offset += n
		return n, nil
	}
	r.blockOnce.Do(func() { close(r.blocked) })
	<-r.release
	return 0, io.EOF
}

func (r *blockingAfterRPCLine) Close() error {
	r.closeCalls.Add(1)
	return errors.New("caller-owned RPC input must not be closed")
}

func (r *blockingAfterRPCLine) releaseRead() {
	r.releaseOnce.Do(func() { close(r.release) })
}
