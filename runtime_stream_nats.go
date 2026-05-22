package ovr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

type defaultStreamReceiver struct {
	mu             sync.Mutex
	redisLastID    map[string]string
	kafkaReaders   map[string]kafkaStreamReader
	kafkaCommit    map[string]bool
	kafkaNewReader kafkaStreamReaderFactory
}

func newDefaultStreamReceiver() *defaultStreamReceiver {
	return &defaultStreamReceiver{
		redisLastID:    make(map[string]string),
		kafkaReaders:   make(map[string]kafkaStreamReader),
		kafkaCommit:    make(map[string]bool),
		kafkaNewReader: newKafkaStreamReader,
	}
}

func (r *defaultStreamReceiver) Receive(ctx context.Context, rawURI string) (streamMessage, error) {
	if err := ctx.Err(); err != nil {
		return streamMessage{}, err
	}
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return streamMessage{}, err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "nats":
		return receiveNATSStream(ctx, parsed)
	case "redis":
		lastID := r.redisReadID(rawURI)
		message, err := receiveRedisStream(ctx, parsed, lastID)
		if err == nil {
			message = r.attachRedisAck(rawURI, message)
		}
		return message, err
	case "kafka":
		return r.receiveKafka(ctx, rawURI, parsed)
	default:
		return streamMessage{}, fmt.Errorf("unsupported stream scheme %q", parsed.Scheme)
	}
}

func (r *defaultStreamReceiver) attachRedisAck(rawURI string, message streamMessage) streamMessage {
	id := strings.TrimSpace(message.ID)
	if id == "" {
		return message
	}
	message.ack = func(context.Context) error {
		r.rememberRedisID(rawURI, id)
		return nil
	}
	message.nack = func(context.Context, error) error {
		r.rememberRedisID(rawURI, redisIDBefore(id))
		return nil
	}
	return message
}

func (r *defaultStreamReceiver) redisReadID(rawURI string) string {
	if r == nil {
		return "$"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisLastID == nil {
		r.redisLastID = make(map[string]string)
	}
	if id := strings.TrimSpace(r.redisLastID[rawURI]); id != "" {
		return id
	}
	return "$"
}

func (r *defaultStreamReceiver) rememberRedisID(rawURI, id string) {
	if r == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisLastID == nil {
		r.redisLastID = make(map[string]string)
	}
	r.redisLastID[rawURI] = id
}

func receiveNATSStream(ctx context.Context, uri *url.URL) (streamMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	address, err := natsQueueAddress(uri)
	if err != nil {
		return streamMessage{}, err
	}

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return streamMessage{}, err
	}
	defer conn.Close()
	return receiveNATSStreamFromConn(ctx, conn, uri)
}

func receiveNATSStreamFromConn(ctx context.Context, conn net.Conn, uri *url.URL) (streamMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	subject, err := natsQueueSubject(uri)
	if err != nil {
		return streamMessage{}, err
	}
	reader := bufio.NewReader(conn)
	infoLine, err := reader.ReadString('\n')
	if err != nil {
		return streamMessage{}, err
	}
	if !strings.HasPrefix(strings.TrimSpace(infoLine), "INFO ") {
		return streamMessage{}, fmt.Errorf("nats stream did not send INFO")
	}

	connectLine, err := natsConnectLine(uri)
	if err != nil {
		return streamMessage{}, err
	}
	if _, err := io.WriteString(conn, connectLine); err != nil {
		return streamMessage{}, err
	}
	if _, err := fmt.Fprintf(conn, "SUB %s 1\r\nPING\r\n", subject); err != nil {
		return streamMessage{}, err
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return streamMessage{}, err
		}
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "MSG "):
			return readNATSStreamMSG(reader, line)
		case line == "PING":
			if _, err := io.WriteString(conn, "PONG\r\n"); err != nil {
				return streamMessage{}, err
			}
		case strings.HasPrefix(line, "-ERR"):
			return streamMessage{}, fmt.Errorf("nats stream error: %s", line)
		}
	}
}

func readNATSStreamMSG(reader *bufio.Reader, line string) (streamMessage, error) {
	subject, size, err := parseNATSMSGLine(line)
	if err != nil {
		return streamMessage{}, err
	}
	payload := make([]byte, size+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return streamMessage{}, err
	}
	if payload[size] != '\r' || payload[size+1] != '\n' {
		return streamMessage{}, fmt.Errorf("nats stream message missing CRLF")
	}
	return streamMessage{
		Body: string(payload[:size]),
		Metadata: map[string]string{
			"subject": subject,
		},
	}, nil
}

func parseNATSMSGLine(line string) (string, int, error) {
	fields := strings.Fields(line)
	if len(fields) != 4 && len(fields) != 5 {
		return "", 0, fmt.Errorf("invalid nats MSG line")
	}
	if fields[0] != "MSG" {
		return "", 0, fmt.Errorf("invalid nats message operation %q", fields[0])
	}
	size, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || size < 0 {
		return "", 0, fmt.Errorf("invalid nats message size")
	}
	return fields[1], size, nil
}
