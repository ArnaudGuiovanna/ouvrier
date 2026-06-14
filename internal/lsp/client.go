package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Client is a hand-rolled LSP JSON-RPC client for gopls.
type Client struct {
	stdin   io.Writer
	writeMu sync.Mutex // serializes all writes to stdin

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse

	diags chan PublishDiagnosticsParams
	enc   PositionEncoding

	cmd *exec.Cmd // non-nil when spawned via New
}

// newClient creates a Client wired to the given writer/reader and starts the
// dispatcher goroutine. Intended for testing and for New.
func newClient(stdin io.Writer, stdout io.Reader) *Client {
	c := &Client{
		stdin:   stdin,
		pending: make(map[int]chan rpcResponse),
		diags:   make(chan PublishDiagnosticsParams, 16),
		enc:     EncodingUTF16,
	}
	go c.dispatch(bufio.NewReader(stdout))
	return c
}

// New spawns gopls at goplsPath, wires up the Client, and runs the handshake.
func New(ctx context.Context, goplsPath, rootDir string) (*Client, error) {
	cmd := exec.CommandContext(ctx, goplsPath)
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start gopls: %w", err)
	}
	c := newClient(stdinPipe, stdoutPipe)
	c.cmd = cmd
	// The gopls PROCESS lifetime is tied to the caller's ctx (long-lived).
	// Only the initialize round-trip is bounded, so a slow handshake fails fast
	// without killing a healthy server mid-session.
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.handshake(initCtx, rootDir); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("lsp: handshake: %w", err)
	}
	return c, nil
}

// dispatch reads framed messages from the server and routes them.
func (c *Client) dispatch(r *bufio.Reader) {
	for {
		raw, err := readMessage(r)
		if err != nil {
			// Server closed or pipe broken — drain all pending channels.
			c.mu.Lock()
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}

		var msg rpcResponse
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch {
		case msg.ID != nil && msg.Method != "":
			// Server-to-client REQUEST: we must reply.
			var result any
			switch msg.Method {
			case "workspace/configuration":
				result = []any{map[string]any{}}
			default:
				result = nil
			}
			resp := rpcResult{JSONRPC: "2.0", ID: *msg.ID, Result: result}
			c.writeMu.Lock()
			_ = writeMessage(c.stdin, resp)
			c.writeMu.Unlock()

		case msg.ID != nil && msg.Method == "":
			// Response to one of our calls.
			c.mu.Lock()
			ch, ok := c.pending[*msg.ID]
			if ok {
				delete(c.pending, *msg.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- msg
			}

		case msg.ID == nil && msg.Method == "textDocument/publishDiagnostics":
			var params PublishDiagnosticsParams
			if err := json.Unmarshal(msg.Params, &params); err == nil {
				select {
				case c.diags <- params:
				default:
				}
			}

		default:
			// Ignore unknown notifications.
		}
	}
}

// call sends a request and waits for the response.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	c.writeMu.Lock()
	writeErr := writeMessage(c.stdin, req)
	c.writeMu.Unlock()
	if writeErr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, writeErr
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("lsp: connection closed")
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("lsp: %s error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// notify sends a notification (no ID, no response expected).
func (c *Client) notify(method string, params any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeMessage(c.stdin, req)
}

// handshake performs LSP initialize + initialized.
func (c *Client) handshake(ctx context.Context, rootDir string) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   URI(rootDir),
		"capabilities": map[string]any{
			"general": map[string]any{
				"positionEncodings": []string{"utf-8", "utf-16"},
			},
			"textDocument": map[string]any{
				"completion": map[string]any{
					"completionItem": map[string]any{
						"snippetSupport": true,
					},
				},
				"hover": map[string]any{
					"contentFormat": []string{"markdown", "plaintext"},
				},
			},
			"window": map[string]any{
				"workDoneProgress": true,
			},
		},
		"initializationOptions": map[string]any{
			"ui.diagnostic.staticcheck": true,
			"completeUnimported":        true,
			"usePlaceholders":           true,
		},
	}

	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return err
	}

	// Parse positionEncoding from result.capabilities.
	var result struct {
		Capabilities struct {
			PositionEncoding string `json:"positionEncoding"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err == nil {
		switch PositionEncoding(result.Capabilities.PositionEncoding) {
		case EncodingUTF8:
			c.enc = EncodingUTF8
		case EncodingUTF16:
			c.enc = EncodingUTF16
		default:
			c.enc = EncodingUTF16
		}
	}

	return c.notify("initialized", map[string]any{})
}

// DidOpen notifies the server that a document was opened.
func (c *Client) DidOpen(uri, languageID, text string, version int) error {
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    version,
			"text":       text,
		},
	})
}

// DidChange notifies the server of a document change.
func (c *Client) DidChange(uri, text string, version int) error {
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument": map[string]any{
			"uri":     uri,
			"version": version,
		},
		"contentChanges": []map[string]any{
			{"text": text},
		},
	})
}

// DidSave notifies the server that a document was saved.
func (c *Client) DidSave(uri string) error {
	return c.notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]any{
			"uri": uri,
		},
	})
}

// Diagnostics returns the channel receiving publish-diagnostics notifications.
func (c *Client) Diagnostics() <-chan PublishDiagnosticsParams {
	return c.diags
}

// Encoding returns the negotiated position encoding.
func (c *Client) Encoding() PositionEncoding {
	return c.enc
}

// Hover requests hover information at the given position.
func (c *Client) Hover(ctx context.Context, uri string, pos Position) (*Hover, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}
	raw, err := c.call(ctx, "textDocument/hover", params)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var h Hover
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Complete requests completion at the given position.
func (c *Client) Complete(ctx context.Context, uri string, pos Position) (*CompletionList, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}
	raw, err := c.call(ctx, "textDocument/completion", params)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var cl CompletionList
	if err := json.Unmarshal(raw, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

// Definition requests the definition location(s) for a symbol.
func (c *Client) Definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}
	raw, err := c.call(ctx, "textDocument/definition", params)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Try array first.
	var locs []Location
	if err := json.Unmarshal(raw, &locs); err == nil {
		return locs, nil
	}
	// Fall back to single location.
	var loc Location
	if err := json.Unmarshal(raw, &loc); err != nil {
		return nil, err
	}
	return []Location{loc}, nil
}

// Shutdown sends shutdown + exit to the server.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.call(ctx, "shutdown", nil)
	if err != nil {
		return err
	}
	return c.notify("exit", nil)
}

// URI converts an absolute filesystem path to a file:// URI string.
func URI(absPath string) string {
	// Ensure forward slashes (no-op on Linux, fixes Windows).
	absPath = filepath.ToSlash(absPath)
	u := &url.URL{Scheme: "file", Path: absPath}
	return u.String()
}

// Discover returns the path to gopls and true if found, or "" and false.
func Discover() (string, bool) {
	if p, err := exec.LookPath("gopls"); err == nil {
		return p, true
	}
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		p := filepath.Join(gobin, "gopls")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	// Try $GOPATH/bin/gopls via `go env GOPATH`.
	out, err := exec.Command("go", "env", "GOPATH").Output()
	if err == nil {
		gopath := strings.TrimSpace(string(out))
		p := filepath.Join(gopath, "bin", "gopls")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}
