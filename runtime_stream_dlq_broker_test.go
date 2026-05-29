package ovr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

// TestStreamDLQRoutesToRealRedisBroker drives a full stream loop with a
// poisoned message and asserts the dead-lettered payload is actually published
// to the configured redis:// DLQ target via XADD on the real RESP transport,
// not merely buffered in an in-memory map.
func TestStreamDLQRoutesToRealRedisBroker(t *testing.T) {
	listener, commands := newRedisCommandRecorder(t, "$15\r\n1700000000000-0\r\n")
	dlqURI := fmt.Sprintf("redis://%s/tickets-dlq", listener.Addr().String())

	stream, _ := events.NewEventStream()
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamDLQ(dlqURI, 2)),
		Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &alwaysFailStreamProvider{}
	receiver := &redeliveringStreamReceiver{
		message: streamMessage{ID: "poison-1", Body: `{"event":"bad"}`},
		limit:   5,
	}
	rt := httpRuntime{
		provider:       prov,
		streamReceiver: receiver,
		eventStream:    stream,
		streamDLQ:      newRoutingStreamDLQ(),
	}
	if err := runStreamLoop(context.Background(), rt, plans[0]); err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}

	cmd := awaitRedisCommand(t, commands)
	if strings.ToUpper(cmd.Args[0]) != "XADD" || cmd.Args[1] != "tickets-dlq" {
		t.Fatalf("command = %v, want XADD tickets-dlq published to real broker", cmd.Args)
	}
	if got := redisFieldValue(cmd.Args[3:], "body"); got != `{"event":"bad"}` {
		t.Fatalf("DLQ body field = %q, want poisoned payload", got)
	}
}

// TestStreamDLQRoutesToRealNATSBroker asserts the dead-lettered payload is
// published to the configured nats:// DLQ subject over the real NATS wire
// protocol.
func TestStreamDLQRoutesToRealNATSBroker(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	published := make(chan string, 1)
	go acceptNATSPublish(listener, published)

	dlqURI := fmt.Sprintf("nats://%s/tickets.dlq", listener.Addr().String())
	stream, _ := events.NewEventStream()
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamDLQ(dlqURI, 2)),
		Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &alwaysFailStreamProvider{}
	receiver := &redeliveringStreamReceiver{
		message: streamMessage{ID: "poison-1", Body: `{"event":"bad"}`},
		limit:   5,
	}
	rt := httpRuntime{
		provider:       prov,
		streamReceiver: receiver,
		eventStream:    stream,
		streamDLQ:      newRoutingStreamDLQ(),
	}
	if err := runStreamLoop(context.Background(), rt, plans[0]); err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}

	select {
	case body := <-published:
		if body != `{"event":"bad"}` {
			t.Fatalf("NATS PUB body = %q, want poisoned payload", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for NATS DLQ publish")
	}
}

// TestStreamDLQRoutesToRealKafkaBroker asserts the dead-lettered payload is
// written to the configured kafka:// DLQ topic via the kafka-go writer.
func TestStreamDLQRoutesToRealKafkaBroker(t *testing.T) {
	writer := &fakeKafkaWriter{}
	prev := kafkaQueueWriterFactory
	kafkaQueueWriterFactory = func(cfg kafkaQueuePublishConfig) (kafkaQueueWriter, error) {
		if cfg.Topic != "tickets.dlq" {
			t.Fatalf("topic = %q, want tickets.dlq", cfg.Topic)
		}
		return writer, nil
	}
	t.Cleanup(func() { kafkaQueueWriterFactory = prev })

	stream, _ := events.NewEventStream()
	plans, err := compilePlans([]Node{
		From(Stream("kafka://tickets"), WorkerPool(1), StreamDLQ("kafka://broker:9092/tickets.dlq", 2)),
		Pipe("summarize", Model("anthropic/claude-haiku-4-5")),
		Sink(Log()),
	})
	if err != nil {
		t.Fatalf("compilePlans returned error: %v", err)
	}
	prov := &alwaysFailStreamProvider{}
	receiver := &redeliveringStreamReceiver{
		message: streamMessage{ID: "poison-1", Body: `{"event":"bad"}`},
		limit:   5,
	}
	rt := httpRuntime{
		provider:       prov,
		streamReceiver: receiver,
		eventStream:    stream,
		streamDLQ:      newRoutingStreamDLQ(),
	}
	if err := runStreamLoop(context.Background(), rt, plans[0]); err != nil {
		t.Fatalf("runStreamLoop returned error: %v", err)
	}

	if len(writer.messages) != 1 {
		t.Fatalf("kafka DLQ messages = %d, want 1", len(writer.messages))
	}
	if string(writer.messages[0].Value) != `{"event":"bad"}` {
		t.Fatalf("kafka DLQ value = %q, want poisoned payload", writer.messages[0].Value)
	}
}

// acceptNATSPublish accepts a single connection, completes the NATS handshake,
// and forwards the body of the first PUB it receives to the channel.
func acceptNATSPublish(listener net.Listener, published chan<- string) {
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "INFO {}\r\n"); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "CONNECT"):
			continue
		case strings.HasPrefix(line, "PING"):
			_, _ = io.WriteString(conn, "PONG\r\n")
		case strings.HasPrefix(line, "PUB "):
			fields := strings.Fields(line)
			size := 0
			if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &size); err != nil {
				return
			}
			payload := make([]byte, size+2)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return
			}
			published <- string(payload[:size])
			// Keep serving: publishNATSQueue sends PING after PUB and blocks on
			// PONG, handled by the PING case below.
		}
	}
}
