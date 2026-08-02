package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestOperateRPCInterruptCancelsInflightPrompt(t *testing.T) {
	dir := t.TempDir()
	model := &rpcBlockingModel{started: make(chan struct{})}
	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{
		Dir: dir, Driver: operate.ManualDriver{}, Model: model, ModelID: "test/rpc-interrupt",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	reader, writer := io.Pipe()
	var out bytes.Buffer
	app := New("dev", WithStreams(reader, &out, io.Discard))
	done := make(chan error, 1)
	go func() {
		done <- app.runOperateRPC(context.Background(), runtime, operateConfig{Dir: dir, Agent: "manual"})
	}()

	if _, err := io.WriteString(writer, `{"id":"prompt-1","type":"prompt","text":"inspect the worker"}`+"\n"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("RPC prompt did not reach model")
	}
	if _, err := io.WriteString(writer, `{"id":"interrupt-1","type":"interrupt","text":"stop now"}`+"\n"); err != nil {
		t.Fatalf("write interrupt: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close RPC input: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runOperateRPC() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RPC server remained blocked after interrupt")
	}

	responses := make(map[string]map[string]any)
	scanner := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	for scanner.Scan() {
		var response map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			t.Fatalf("decode response %q: %v", scanner.Bytes(), err)
		}
		id, _ := response["id"].(string)
		responses[id] = response
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if got := responses["prompt-1"]["type"]; got != "error" {
		t.Fatalf("prompt response = %#v", responses["prompt-1"])
	}
	if got := responses["interrupt-1"]["type"]; got != "turn" {
		t.Fatalf("interrupt response = %#v", responses["interrupt-1"])
	}
}

type rpcBlockingModel struct {
	started chan struct{}
	once    sync.Once
}

func (m *rpcBlockingModel) Complete(ctx context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.once.Do(func() { close(m.started) })
	<-ctx.Done()
	return provider.Response{}, ctx.Err()
}
