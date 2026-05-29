package ovr

import (
	"context"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
)

type fakeKafkaWriter struct {
	messages []kafka.Message
	closed   bool
	writeErr error
}

func (w *fakeKafkaWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if w.writeErr != nil {
		return w.writeErr
	}
	w.messages = append(w.messages, msgs...)
	return nil
}

func (w *fakeKafkaWriter) Close() error {
	w.closed = true
	return nil
}

func TestPublishKafkaQueueWritesMessage(t *testing.T) {
	writer := &fakeKafkaWriter{}
	prev := kafkaQueueWriterFactory
	kafkaQueueWriterFactory = func(cfg kafkaQueuePublishConfig) (kafkaQueueWriter, error) {
		if cfg.Topic != "results" {
			t.Fatalf("topic = %q, want results", cfg.Topic)
		}
		if len(cfg.Brokers) != 1 || cfg.Brokers[0] != "broker:9092" {
			t.Fatalf("brokers = %v, want [broker:9092]", cfg.Brokers)
		}
		return writer, nil
	}
	t.Cleanup(func() { kafkaQueueWriterFactory = prev })

	if err := publishQueue(context.Background(), "kafka://broker:9092/results", `{"status":"ok"}`); err != nil {
		t.Fatalf("publishQueue returned error: %v", err)
	}
	if len(writer.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(writer.messages))
	}
	if string(writer.messages[0].Value) != `{"status":"ok"}` {
		t.Fatalf("value = %q, want payload", writer.messages[0].Value)
	}
	if !writer.closed {
		t.Fatal("writer was not closed")
	}
}

func TestPublishKafkaQueuePropagatesIdempotencyKeyAsMessageKey(t *testing.T) {
	writer := &fakeKafkaWriter{}
	prev := kafkaQueueWriterFactory
	kafkaQueueWriterFactory = func(cfg kafkaQueuePublishConfig) (kafkaQueueWriter, error) {
		return writer, nil
	}
	t.Cleanup(func() { kafkaQueueWriterFactory = prev })

	if err := publishQueue(context.Background(), "kafka://broker:9092/results?idempotency_key=abc-123", "payload"); err != nil {
		t.Fatalf("publishQueue returned error: %v", err)
	}
	if len(writer.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(writer.messages))
	}
	if string(writer.messages[0].Key) != "abc-123" {
		t.Fatalf("key = %q, want abc-123", writer.messages[0].Key)
	}
}

func TestPublishKafkaQueueReturnsWriteError(t *testing.T) {
	writer := &fakeKafkaWriter{writeErr: errors.New("boom")}
	prev := kafkaQueueWriterFactory
	kafkaQueueWriterFactory = func(cfg kafkaQueuePublishConfig) (kafkaQueueWriter, error) {
		return writer, nil
	}
	t.Cleanup(func() { kafkaQueueWriterFactory = prev })

	if err := publishQueue(context.Background(), "kafka://broker:9092/results", "payload"); err == nil {
		t.Fatal("publishQueue returned nil, want write error")
	}
}

func TestPublishKafkaQueueRequiresTopic(t *testing.T) {
	if err := publishQueue(context.Background(), "kafka://broker:9092/", "payload"); err == nil {
		t.Fatal("publishQueue returned nil, want topic-required error")
	}
}
