package ovr

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type redisRESP struct {
	kind  byte
	text  string
	array []redisRESP
	null  bool
}

func receiveRedisStream(ctx context.Context, uri *url.URL, lastID string) (streamMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := dialRedis(ctx, uri)
	if err != nil {
		return streamMessage{}, err
	}
	defer conn.Close()
	return receiveRedisStreamFromConn(ctx, conn, uri, lastID)
}

func receiveRedisStreamFromConn(ctx context.Context, conn net.Conn, uri *url.URL, lastID string) (streamMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	stream, err := redisStreamName(uri)
	if err != nil {
		return streamMessage{}, err
	}
	reader := bufio.NewReader(conn)
	if err := redisAuthenticate(conn, reader, uri); err != nil {
		return streamMessage{}, err
	}
	readID := redisStreamReadID(lastID)
	if err := writeRedisCommand(conn, "XREAD", "BLOCK", "0", "COUNT", "1", "STREAMS", stream, readID); err != nil {
		return streamMessage{}, err
	}
	value, err := readRedisRESP(reader)
	if err != nil {
		return streamMessage{}, err
	}
	if err := redisResponseError(value); err != nil {
		return streamMessage{}, err
	}
	return redisStreamMessage(value)
}

func redisStreamReadID(lastID string) string {
	lastID = strings.TrimSpace(lastID)
	if lastID == "" {
		return "$"
	}
	return lastID
}

func redisIDBefore(id string) string {
	msRaw, seqRaw, ok := strings.Cut(strings.TrimSpace(id), "-")
	if !ok {
		return "0-0"
	}
	ms, msErr := strconv.ParseUint(msRaw, 10, 64)
	seq, seqErr := strconv.ParseUint(seqRaw, 10, 64)
	if msErr != nil || seqErr != nil {
		return "0-0"
	}
	if seq > 0 {
		return fmt.Sprintf("%d-%d", ms, seq-1)
	}
	if ms == 0 {
		return "0-0"
	}
	return fmt.Sprintf("%d-%d", ms-1, ^uint64(0))
}

func redisAuthenticate(conn net.Conn, reader *bufio.Reader, uri *url.URL) error {
	if uri.User == nil {
		return nil
	}
	user := uri.User.Username()
	password, hasPassword := uri.User.Password()
	if user == "" && !hasPassword {
		return nil
	}
	args := []string{"AUTH"}
	if user != "" && hasPassword {
		args = append(args, user, password)
	} else if hasPassword {
		args = append(args, password)
	} else {
		args = append(args, user)
	}
	if err := writeRedisCommand(conn, args...); err != nil {
		return err
	}
	value, err := readRedisRESP(reader)
	if err != nil {
		return err
	}
	return redisResponseError(value)
}

func redisStreamName(uri *url.URL) (string, error) {
	stream := strings.Trim(strings.TrimSpace(uri.Path), "/")
	if stream == "" {
		stream = strings.TrimSpace(uri.Query().Get("stream"))
	}
	if stream == "" {
		return "", fmt.Errorf("redis stream name is required")
	}
	if strings.ContainsAny(stream, " \t\r\n") {
		return "", fmt.Errorf("redis stream name is invalid")
	}
	return stream, nil
}

func redisStreamAddress(uri *url.URL) (string, error) {
	host := uri.Hostname()
	if host == "" {
		return "", fmt.Errorf("redis stream host is required")
	}
	port := uri.Port()
	if port == "" {
		port = "6379"
	}
	return net.JoinHostPort(host, port), nil
}

// dialRedis opens a connection to the Redis endpoint named by uri. For the
// rediss:// scheme the socket is wrapped in TLS (verifying the server
// certificate against the URI host) so the subsequent AUTH command and payload
// are encrypted; the plain redis:// scheme dials cleartext TCP as before. This
// closes the H6 downgrade where rediss:// silently used an unencrypted socket,
// leaking the AUTH password and message bodies. Both the stream consumer and
// the queue push terminal dial through here so the two paths stay consistent.
func dialRedis(ctx context.Context, uri *url.URL) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	address, err := redisStreamAddress(uri)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(uri.Scheme, "rediss") {
		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: defaultQueuePublishTimeout},
			Config:    &tls.Config{ServerName: uri.Hostname()},
		}
		return dialer.DialContext(ctx, "tcp", address)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

func writeRedisCommand(writer io.Writer, args ...string) error {
	if _, err := fmt.Fprintf(writer, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, arg := range args {
		if _, err := fmt.Fprintf(writer, "$%d\r\n%s\r\n", len(arg), arg); err != nil {
			return err
		}
	}
	return nil
}

func readRedisRESP(reader *bufio.Reader) (redisRESP, error) {
	kind, err := reader.ReadByte()
	if err != nil {
		return redisRESP{}, err
	}
	switch kind {
	case '+', '-', ':':
		line, err := readRESPLine(reader)
		return redisRESP{kind: kind, text: line}, err
	case '$':
		size, err := readRESPSize(reader)
		if err != nil || size < 0 {
			return redisRESP{kind: kind, null: size < 0}, err
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return redisRESP{}, err
		}
		if payload[size] != '\r' || payload[size+1] != '\n' {
			return redisRESP{}, fmt.Errorf("redis bulk string missing CRLF")
		}
		return redisRESP{kind: kind, text: string(payload[:size])}, nil
	case '*':
		size, err := readRESPSize(reader)
		if err != nil || size < 0 {
			return redisRESP{kind: kind, null: size < 0}, err
		}
		values := make([]redisRESP, size)
		for i := range values {
			values[i], err = readRedisRESP(reader)
			if err != nil {
				return redisRESP{}, err
			}
		}
		return redisRESP{kind: kind, array: values}, nil
	default:
		return redisRESP{}, fmt.Errorf("unsupported redis RESP kind %q", kind)
	}
}

func readRESPLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func readRESPSize(reader *bufio.Reader) (int, error) {
	line, err := readRESPLine(reader)
	if err != nil {
		return 0, err
	}
	size, err := strconv.Atoi(line)
	if err != nil {
		return 0, fmt.Errorf("invalid redis RESP size")
	}
	return size, nil
}

func redisResponseError(value redisRESP) error {
	if value.kind == '-' {
		return fmt.Errorf("redis stream error: %s", value.text)
	}
	return nil
}

func redisStreamMessage(value redisRESP) (streamMessage, error) {
	if value.null {
		return streamMessage{}, io.EOF
	}
	if len(value.array) == 0 || len(value.array[0].array) < 2 {
		return streamMessage{}, fmt.Errorf("redis stream response is invalid")
	}
	streamName := redisText(value.array[0].array[0])
	entries := value.array[0].array[1].array
	if len(entries) == 0 || len(entries[0].array) < 2 {
		return streamMessage{}, fmt.Errorf("redis stream response has no entries")
	}
	entry := entries[0].array
	id := redisText(entry[0])
	fields := entry[1].array
	body, err := redisStreamBody(fields)
	if err != nil {
		return streamMessage{}, err
	}
	return streamMessage{
		ID:   id,
		Body: body,
		Metadata: map[string]string{
			"stream": streamName,
		},
	}, nil
}

func redisStreamBody(fields []redisRESP) (string, error) {
	if len(fields)%2 != 0 {
		return "", fmt.Errorf("redis stream entry fields are invalid")
	}
	object := make(map[string]string, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		key := redisText(fields[i])
		value := redisText(fields[i+1])
		if key == "body" {
			return value, nil
		}
		object[key] = value
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func redisText(value redisRESP) string {
	return value.text
}
