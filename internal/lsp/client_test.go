package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// fakeServer simulates a minimal gopls over two pipes.
// It reads requests from r and queues outgoing frames via a writer goroutine
// so that reads and writes never deadlock.
//
// enc is what the fake advertises in initialize.capabilities.positionEncoding.
// configReplied is closed when the fake receives the client's reply to the
// workspace/configuration request it sends after "initialized".
// diagSent is closed after it sends textDocument/publishDiagnostics.
type fakeServer struct {
	t             *testing.T
	enc           string
	outCh         chan any // serialized write queue
	configReplied chan struct{}
	diagSent      chan struct{}
}

func newFakeServer(t *testing.T, w io.Writer, enc string, configReplied, diagSent chan struct{}) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		t:             t,
		enc:           enc,
		outCh:         make(chan any, 64),
		configReplied: configReplied,
		diagSent:      diagSent,
	}
	// Writer goroutine: serializes all writes to w.
	go func() {
		for msg := range fs.outCh {
			if err := writeMessage(w, msg); err != nil {
				return
			}
		}
	}()
	return fs
}

func (fs *fakeServer) send(v any) {
	fs.outCh <- v
}

func (fs *fakeServer) run(r io.Reader) {
	fs.t.Helper()
	br := bufio.NewReader(r)
	const cfgID = 1000
	cfgSent := false
	cfgReplyReceived := false

	for {
		raw, err := readMessage(br)
		if err != nil {
			close(fs.outCh)
			return
		}

		var msg rpcResponse
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		// Check if this is the client's reply to our workspace/configuration request.
		if cfgSent && !cfgReplyReceived && msg.ID != nil && *msg.ID == cfgID && msg.Method == "" {
			cfgReplyReceived = true
			if fs.configReplied != nil {
				close(fs.configReplied)
			}
			continue
		}

		switch msg.Method {
		case "initialize":
			result := map[string]any{
				"capabilities": map[string]any{
					"positionEncoding": fs.enc,
				},
			}
			fs.send(rpcResult{JSONRPC: "2.0", ID: *msg.ID, Result: result})

		case "initialized":
			id := cfgID
			req := rpcRequest{
				JSONRPC: "2.0",
				ID:      &id,
				Method:  "workspace/configuration",
				Params:  map[string]any{"items": []any{}},
			}
			fs.send(req)
			cfgSent = true

		case "textDocument/didOpen":
			notif := rpcRequest{
				JSONRPC: "2.0",
				Method:  "textDocument/publishDiagnostics",
				Params: map[string]any{
					"uri": "file:///fake/file.go",
					"diagnostics": []map[string]any{
						{
							"range": map[string]any{
								"start": map[string]any{"line": 0, "character": 0},
								"end":   map[string]any{"line": 0, "character": 10},
							},
							"message": "undefined: notifier",
						},
					},
				},
			}
			fs.send(notif)
			if fs.diagSent != nil {
				close(fs.diagSent)
			}

		case "shutdown":
			fs.send(rpcResult{JSONRPC: "2.0", ID: *msg.ID, Result: nil})

		case "exit":
			close(fs.outCh)
			return
		}
	}
}

// newTestPair creates a connected client+fake-server pair.
// enc is what the fake will advertise.
func newTestPair(t *testing.T, enc string) (
	client *Client,
	configReplied chan struct{},
	diagSent chan struct{},
) {
	t.Helper()

	// clientStdin: client writes here; fake reads from it.
	clientStdinReader, clientStdinWriter := io.Pipe()
	// serverStdout: fake writes here; client reads from it.
	serverStdoutReader, serverStdoutWriter := io.Pipe()

	configReplied = make(chan struct{})
	diagSent = make(chan struct{})

	fs := newFakeServer(t, serverStdoutWriter, enc, configReplied, diagSent)
	go fs.run(clientStdinReader)

	c := newClient(clientStdinWriter, serverStdoutReader)
	t.Cleanup(func() {
		clientStdinWriter.Close()
		serverStdoutWriter.Close()
	})
	return c, configReplied, diagSent
}

func TestHandshakeUTF8(t *testing.T) {
	c, _, _ := newTestPair(t, "utf-8")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.handshake(ctx, "/tmp"); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if c.Encoding() != EncodingUTF8 {
		t.Errorf("want utf-8, got %q", c.Encoding())
	}
}

func TestHandshakeUTF16(t *testing.T) {
	c, _, _ := newTestPair(t, "utf-16")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.handshake(ctx, "/tmp"); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if c.Encoding() != EncodingUTF16 {
		t.Errorf("want utf-16, got %q", c.Encoding())
	}
}

func TestDiagnosticsAfterDidOpen(t *testing.T) {
	c, _, _ := newTestPair(t, "utf-8")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.handshake(ctx, "/tmp"); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := c.DidOpen("file:///fake/file.go", "go", "package main", 1); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	select {
	case diag := <-c.Diagnostics():
		found := false
		for _, d := range diag.Diagnostics {
			if d.Message == "undefined: notifier" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected diagnostic 'undefined: notifier', got %+v", diag.Diagnostics)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for diagnostics")
	}
}

func TestServerToClientConfigurationAnswered(t *testing.T) {
	c, configReplied, _ := newTestPair(t, "utf-8")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.handshake(ctx, "/tmp"); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	select {
	case <-configReplied:
		// Good — the fake confirmed the client answered id=1000.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: client did not reply to workspace/configuration")
	}
}
