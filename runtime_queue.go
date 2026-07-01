package ovr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultQueuePublishTimeout = 5 * time.Second

// egressTimeout bounds a single outbound HTTP push (a Push webhook terminal, or
// an HTTP/SQS queue terminal) end to end. The stream, cron and durable callers
// may pass a context without a deadline, and http.DefaultClient has no timeout,
// so a black-holed or slow endpoint would otherwise pin the worker goroutine —
// and the consumer pool slot it holds — indefinitely (audit H5). It is a var so
// tests can shorten it. The socket-level queue backends (NATS/Redis) already
// bound themselves through queuePublishDeadline, so this covers only the HTTP
// egress paths.
var egressTimeout = 30 * time.Second

// egressHTTPClient is the dedicated client for outbound HTTP pushes. Unlike
// http.DefaultClient it carries a total-request Timeout as a hard backstop, in
// addition to the per-push deadline applied via egressContext.
var egressHTTPClient = &http.Client{Timeout: egressTimeout}

// egressContext derives a context carrying the egress deadline. A caller's
// earlier deadline is preserved; a caller with no deadline gets egressTimeout.
func egressContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, egressTimeout)
}

func publishQueue(ctx context.Context, rawURI, output string) error {
	parsed, err := url.Parse(rawURI)
	if err != nil {
		return err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return postHTTPQueue(ctx, rawURI, output)
	case "nats":
		return publishNATSQueue(ctx, parsed, output)
	case "kafka":
		return publishKafkaQueue(ctx, parsed, output)
	case "redis", "rediss":
		return publishRedisQueue(ctx, parsed, output)
	case "sqs":
		return publishSQSQueue(ctx, parsed, output)
	default:
		return fmt.Errorf("unsupported queue scheme %q", parsed.Scheme)
	}
}

// queueIdempotencyKey returns the idempotency key carried by the queue URI, if
// any. Callers propagate it to the target protocol where supported (Kafka
// message key, SQS MessageDeduplicationId, Redis stream field).
func queueIdempotencyKey(uri *url.URL) string {
	if uri == nil {
		return ""
	}
	query := uri.Query()
	for _, name := range []string{"idempotency_key", "idempotency-key", "idempotencyKey"} {
		if value := strings.TrimSpace(query.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func postHTTPQueue(ctx context.Context, rawURL, output string) error {
	ctx, cancel := egressContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewBufferString(output))
	if err != nil {
		return err
	}
	if json.Valid([]byte(output)) {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	}
	resp, err := egressHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("queue push returned status %d", resp.StatusCode)
	}
	return nil
}

func publishNATSQueue(ctx context.Context, uri *url.URL, output string) error {
	subject, err := natsQueueSubject(uri)
	if err != nil {
		return err
	}
	address, err := natsQueueAddress(uri)
	if err != nil {
		return err
	}

	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(queuePublishDeadline(ctx)); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	infoLine, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(infoLine), "INFO ") {
		return fmt.Errorf("nats queue did not send INFO")
	}

	connectLine, err := natsConnectLine(uri)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(conn, connectLine); err != nil {
		return err
	}
	payload := []byte(output)
	if _, err := fmt.Fprintf(conn, "PUB %s %d\r\n", subject, len(payload)); err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "\r\nPING\r\n"); err != nil {
		return err
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "PONG":
			return nil
		case strings.HasPrefix(line, "-ERR"):
			return fmt.Errorf("nats queue error: %s", line)
		}
	}
}

func natsQueueSubject(uri *url.URL) (string, error) {
	subject := strings.Trim(strings.TrimSpace(uri.Path), "/")
	if subject == "" {
		subject = strings.TrimSpace(uri.Query().Get("subject"))
	}
	if subject == "" {
		return "", fmt.Errorf("nats queue subject is required")
	}
	if strings.ContainsAny(subject, " \t\r\n") {
		return "", fmt.Errorf("nats queue subject is invalid")
	}
	return subject, nil
}

func natsQueueAddress(uri *url.URL) (string, error) {
	host := uri.Hostname()
	if host == "" {
		return "", fmt.Errorf("nats queue host is required")
	}
	port := uri.Port()
	if port == "" {
		port = "4222"
	}
	return net.JoinHostPort(host, port), nil
}

func natsConnectLine(uri *url.URL) (string, error) {
	payload := map[string]any{
		"lang":     "go",
		"name":     "ouvrier",
		"protocol": 1,
		"verbose":  false,
		"pedantic": false,
	}
	if uri.User != nil {
		if user := uri.User.Username(); user != "" {
			payload["user"] = user
		}
		if password, ok := uri.User.Password(); ok {
			payload["pass"] = password
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return "CONNECT " + string(encoded) + "\r\n", nil
}

func queuePublishDeadline(ctx context.Context) time.Time {
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok {
			return deadline
		}
	}
	return time.Now().Add(defaultQueuePublishTimeout)
}
