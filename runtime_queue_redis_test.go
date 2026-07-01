package ovr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recordedRedisCommand struct {
	Args []string
}

// newRedisCommandRecorder starts a minimal RESP server that reads a single
// command, records it, replies with a generic OK-ish response, and returns the
// redis:// URI plus a channel that receives the parsed command.
func newRedisCommandRecorder(t *testing.T, reply string) (net.Listener, <-chan recordedRedisCommand) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	commands := make(chan recordedRedisCommand, 4)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			args, err := readRESPCommand(reader)
			if err != nil {
				return
			}
			commands <- recordedRedisCommand{Args: args}
			if _, err := io.WriteString(conn, reply); err != nil {
				return
			}
		}
	}()
	return listener, commands
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	prefix, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimRight(prefix, "\r\n")
	if len(prefix) == 0 || prefix[0] != '*' {
		return nil, fmt.Errorf("expected array, got %q", prefix)
	}
	var count int
	if _, err := fmt.Sscanf(prefix[1:], "%d", &count); err != nil {
		return nil, err
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimRight(sizeLine, "\r\n")
		if len(sizeLine) == 0 || sizeLine[0] != '$' {
			return nil, fmt.Errorf("expected bulk string, got %q", sizeLine)
		}
		var size int
		if _, err := fmt.Sscanf(sizeLine[1:], "%d", &size); err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(reader, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func awaitRedisCommand(t *testing.T, commands <-chan recordedRedisCommand) recordedRedisCommand {
	t.Helper()
	select {
	case cmd := <-commands:
		return cmd
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for redis command")
		return recordedRedisCommand{}
	}
}

func TestPublishRedisQueueUsesXADD(t *testing.T) {
	listener, commands := newRedisCommandRecorder(t, "$15\r\n1700000000000-0\r\n")
	uri := fmt.Sprintf("redis://%s/results", listener.Addr().String())

	if err := publishQueue(context.Background(), uri, `{"status":"ok"}`); err != nil {
		t.Fatalf("publishQueue returned error: %v", err)
	}

	cmd := awaitRedisCommand(t, commands)
	if len(cmd.Args) < 5 || strings.ToUpper(cmd.Args[0]) != "XADD" {
		t.Fatalf("command = %v, want XADD", cmd.Args)
	}
	if cmd.Args[1] != "results" {
		t.Fatalf("stream = %q, want results", cmd.Args[1])
	}
	if cmd.Args[2] != "*" {
		t.Fatalf("id arg = %q, want *", cmd.Args[2])
	}
	body := redisFieldValue(cmd.Args[3:], "body")
	if body != `{"status":"ok"}` {
		t.Fatalf("body field = %q, want payload", body)
	}
}

func TestPublishRedisQueuePropagatesIdempotencyKey(t *testing.T) {
	listener, commands := newRedisCommandRecorder(t, "$15\r\n1700000000000-0\r\n")
	uri := fmt.Sprintf("redis://%s/results?idempotency_key=abc-123", listener.Addr().String())

	if err := publishQueue(context.Background(), uri, `{"status":"ok"}`); err != nil {
		t.Fatalf("publishQueue returned error: %v", err)
	}

	cmd := awaitRedisCommand(t, commands)
	if got := redisFieldValue(cmd.Args[3:], "idempotency_key"); got != "abc-123" {
		t.Fatalf("idempotency_key field = %q, want abc-123", got)
	}
}

func TestPublishRedisQueueReturnsErrorOnRESPError(t *testing.T) {
	listener, _ := newRedisCommandRecorder(t, "-ERR bad stream\r\n")
	uri := fmt.Sprintf("redis://%s/results", listener.Addr().String())

	if err := publishQueue(context.Background(), uri, "payload"); err == nil {
		t.Fatal("publishQueue returned nil, want error")
	}
}

func TestPublishRedisQueueRequiresStream(t *testing.T) {
	if err := publishQueue(context.Background(), "redis://127.0.0.1:6379/", "payload"); err == nil {
		t.Fatal("publishQueue returned nil, want stream-required error")
	}
}

// TestPublishRedisQueueRejectsCleartextForRedissScheme proves the H6 fix: the
// recorder speaks plaintext RESP, so a rediss:// push must attempt a TLS
// handshake against it and fail rather than sending AUTH and the payload over
// an unencrypted socket. The same endpoint over redis:// succeeds in the other
// tests, so this pins the scheme-dependent behavior.
func TestPublishRedisQueueRejectsCleartextForRedissScheme(t *testing.T) {
	listener, _ := newRedisCommandRecorder(t, "$15\r\n1700000000000-0\r\n")
	uri := fmt.Sprintf("rediss://user:secret@%s/results", listener.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := publishQueue(ctx, uri, `{"status":"ok"}`); err == nil {
		t.Fatal("publishQueue over rediss:// to a plaintext server returned nil; the TLS handshake was skipped (cleartext downgrade)")
	}
}

func TestNewHTTPHandlerPushesPipelineOutputToRedisQueue(t *testing.T) {
	listener, commands := newRedisCommandRecorder(t, "$15\r\n1700000000000-0\r\n")
	uri := fmt.Sprintf("redis://%s/results", listener.Addr().String())

	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /events"),
		Push(Queue(uri)),
	}, httpRuntime{toolExecutor: outputAllowedExecutor("queue")})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"event":"created"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	cmd := awaitRedisCommand(t, commands)
	if strings.ToUpper(cmd.Args[0]) != "XADD" || cmd.Args[1] != "results" {
		t.Fatalf("command = %v, want XADD results", cmd.Args)
	}
	if got := redisFieldValue(cmd.Args[3:], "body"); got != `{"event":"created"}` {
		t.Fatalf("body field = %q, want event payload", got)
	}
}

func redisFieldValue(fields []string, key string) string {
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == key {
			return fields[i+1]
		}
	}
	return ""
}
