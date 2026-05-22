package ovr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestDefaultStreamReceiverReceivesRedisStreamMessage(t *testing.T) {
	uri, err := url.Parse("redis://127.0.0.1:6379/tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	client, server := net.Pipe()
	defer client.Close()
	done := serveTestRedisConn(t, server, nil, []string{"event", "created"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := receiveRedisStreamFromConn(ctx, client, uri, "")
	if err != nil {
		t.Fatalf("receiveRedisStreamFromConn returned error: %v", err)
	}
	if message.ID != "1747840000000-0" {
		t.Fatalf("id = %q, want Redis stream ID", message.ID)
	}
	if message.Body != `{"event":"created"}` {
		t.Fatalf("body = %q, want Redis field JSON", message.Body)
	}
	if message.Metadata["stream"] != "tickets" {
		t.Fatalf("stream metadata = %q, want tickets", message.Metadata["stream"])
	}
	waitTestRedisServer(t, done)
}

func TestRedisStreamReceiverUsesBodyFieldAsRawMessageBody(t *testing.T) {
	uri, err := url.Parse("redis://127.0.0.1:6379/tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	client, server := net.Pipe()
	defer client.Close()
	done := serveTestRedisConn(t, server, nil, []string{"body", `{"event":"created"}`})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	message, err := receiveRedisStreamFromConn(ctx, client, uri, "")
	if err != nil {
		t.Fatalf("receiveRedisStreamFromConn returned error: %v", err)
	}
	if message.Body != `{"event":"created"}` {
		t.Fatalf("body = %q, want raw body field", message.Body)
	}
	waitTestRedisServer(t, done)
}

func TestRedisStreamReceiverAuthenticatesWhenURIHasCredentials(t *testing.T) {
	uri, err := url.Parse("redis://user:secret@127.0.0.1:6379/tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	client, server := net.Pipe()
	defer client.Close()
	done := serveTestRedisConn(t, server, []string{"AUTH", "user", "secret"}, []string{"event", "created"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := receiveRedisStreamFromConn(ctx, client, uri, ""); err != nil {
		t.Fatalf("receiveRedisStreamFromConn returned error: %v", err)
	}
	waitTestRedisServer(t, done)
}

func TestRedisStreamReceiverUsesLastIDForNextRead(t *testing.T) {
	uri, err := url.Parse("redis://127.0.0.1:6379/tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	client, server := net.Pipe()
	defer client.Close()
	done := serveTestRedisConnWithReadID(t, server, "1747840000000-0", []string{"event", "created"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := receiveRedisStreamFromConn(ctx, client, uri, "1747840000000-0"); err != nil {
		t.Fatalf("receiveRedisStreamFromConn returned error: %v", err)
	}
	waitTestRedisServer(t, done)
}

func TestDefaultStreamReceiverTracksRedisLastIDPerURI(t *testing.T) {
	receiver := newDefaultStreamReceiver()
	rawURI := "redis://127.0.0.1:6379/tickets"

	if got := receiver.redisReadID(rawURI); got != "$" {
		t.Fatalf("initial read ID = %q, want $", got)
	}
	receiver.rememberRedisID(rawURI, "1747840000000-0")
	if got := receiver.redisReadID(rawURI); got != "1747840000000-0" {
		t.Fatalf("next read ID = %q, want last delivered ID", got)
	}
	if got := receiver.redisReadID("redis://127.0.0.1:6379/other"); got != "$" {
		t.Fatalf("other stream read ID = %q, want independent $", got)
	}
}

func TestDefaultStreamReceiverAdvancesRedisLastIDOnlyOnAck(t *testing.T) {
	receiver := newDefaultStreamReceiver()
	rawURI := "redis://127.0.0.1:6379/tickets"
	message := receiver.attachRedisAck(rawURI, streamMessage{ID: "1747840000000-0"})

	if got := receiver.redisReadID(rawURI); got != "$" {
		t.Fatalf("read ID before ack = %q, want $", got)
	}
	if err := message.ack(context.Background()); err != nil {
		t.Fatalf("ack returned error: %v", err)
	}
	if got := receiver.redisReadID(rawURI); got != "1747840000000-0" {
		t.Fatalf("read ID after ack = %q, want delivered ID", got)
	}
}

func TestDefaultStreamReceiverRewindsRedisLastIDOnNack(t *testing.T) {
	receiver := newDefaultStreamReceiver()
	rawURI := "redis://127.0.0.1:6379/tickets"
	message := receiver.attachRedisAck(rawURI, streamMessage{ID: "1747840000000-0"})

	if err := message.nack(context.Background(), fmt.Errorf("delivery failed")); err != nil {
		t.Fatalf("nack returned error: %v", err)
	}
	if got, want := receiver.redisReadID(rawURI), "1747839999999-18446744073709551615"; got != want {
		t.Fatalf("read ID after nack = %q, want %q", got, want)
	}
}

func serveTestRedisConn(t *testing.T, conn net.Conn, wantAuth []string, fields []string) <-chan error {
	return serveTestRedisConnWithOptions(t, conn, wantAuth, "$", fields)
}

func serveTestRedisConnWithReadID(t *testing.T, conn net.Conn, readID string, fields []string) <-chan error {
	return serveTestRedisConnWithOptions(t, conn, nil, readID, fields)
}

func serveTestRedisConnWithOptions(t *testing.T, conn net.Conn, wantAuth []string, readID string, fields []string) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if wantAuth != nil {
			command, err := readRedisRESP(reader)
			if err != nil {
				done <- err
				return
			}
			if got := redisCommandParts(command); !reflect.DeepEqual(got, wantAuth) {
				done <- fmt.Errorf("auth command = %#v, want %#v", got, wantAuth)
				return
			}
			if _, err := io.WriteString(conn, "+OK\r\n"); err != nil {
				done <- err
				return
			}
		}
		command, err := readRedisRESP(reader)
		if err != nil {
			done <- err
			return
		}
		wantXRead := []string{"XREAD", "BLOCK", "0", "COUNT", "1", "STREAMS", "tickets", readID}
		if got := redisCommandParts(command); !reflect.DeepEqual(got, wantXRead) {
			done <- fmt.Errorf("xread command = %#v, want %#v", got, wantXRead)
			return
		}
		if err := writeTestRedisXReadResponse(conn, "tickets", "1747840000000-0", fields); err != nil {
			done <- err
			return
		}
	}()
	return done
}

func redisCommandParts(value redisRESP) []string {
	parts := make([]string, 0, len(value.array))
	for _, item := range value.array {
		parts = append(parts, item.text)
	}
	return parts
}

func writeTestRedisXReadResponse(writer io.Writer, stream, id string, fields []string) error {
	if len(fields)%2 != 0 {
		return fmt.Errorf("fields must be pairs")
	}
	if _, err := fmt.Fprintf(writer, "*1\r\n*2\r\n$%d\r\n%s\r\n*1\r\n*2\r\n$%d\r\n%s\r\n*%d\r\n",
		len(stream), stream, len(id), id, len(fields)); err != nil {
		return err
	}
	for _, field := range fields {
		if _, err := fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(field), field); err != nil {
			return err
		}
	}
	return nil
}

func waitTestRedisServer(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err, ok := <-done:
		if ok && err != nil {
			t.Fatalf("test Redis server returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("test Redis server did not finish")
	}
}
