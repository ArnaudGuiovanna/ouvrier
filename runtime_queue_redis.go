package ovr

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"strings"
)

// publishRedisQueue appends the outcome to a Redis stream via XADD, following
// the RESP client style of runtime_stream_redis.go. XADD (rather than
// LPUSH/PUBLISH) is used so the push terminal mirrors the redis:// stream
// trigger: the body is stored under the "body" field and consumers read it with
// XREAD. When the queue URI carries an idempotency key it is stored alongside
// the body as the "idempotency_key" field.
func publishRedisQueue(ctx context.Context, uri *url.URL, output string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	stream, err := redisStreamName(uri)
	if err != nil {
		return err
	}

	conn, err := dialRedis(ctx, uri)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(queuePublishDeadline(ctx)); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	if err := redisAuthenticate(conn, reader, uri); err != nil {
		return err
	}

	args := []string{"XADD", stream, "*", "body", output}
	if key := queueIdempotencyKey(uri); key != "" {
		args = append(args, "idempotency_key", key)
	}
	if err := writeRedisCommand(conn, args...); err != nil {
		return err
	}
	value, err := readRedisRESP(reader)
	if err != nil {
		return err
	}
	if err := redisResponseError(value); err != nil {
		return err
	}
	if value.null || (strings.TrimSpace(value.text) == "" && len(value.array) == 0) {
		return fmt.Errorf("redis queue push returned empty reply")
	}
	return nil
}
