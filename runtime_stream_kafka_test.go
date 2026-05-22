package ovr

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/segmentio/kafka-go"
)

func TestKafkaStreamConfigFromURIUsesEnvBrokersForTopicOnlyURI(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "broker-a:9092, broker-b:9092")
	uri, err := url.Parse("kafka://tickets?group=ouvrier&start=first")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	config, err := kafkaStreamConfigFromURI(uri)
	if err != nil {
		t.Fatalf("kafkaStreamConfigFromURI returned error: %v", err)
	}
	if !reflect.DeepEqual(config.Brokers, []string{"broker-a:9092", "broker-b:9092"}) {
		t.Fatalf("brokers = %#v, want env brokers", config.Brokers)
	}
	if config.Topic != "tickets" {
		t.Fatalf("topic = %q, want tickets", config.Topic)
	}
	if config.GroupID != "ouvrier" {
		t.Fatalf("group = %q, want ouvrier", config.GroupID)
	}
	if config.StartOffset != kafka.FirstOffset {
		t.Fatalf("start offset = %d, want first offset", config.StartOffset)
	}
}

func TestKafkaStreamConfigFromURIUsesBrokerHostAndPathTopic(t *testing.T) {
	uri, err := url.Parse("kafka://broker.example.com/tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	config, err := kafkaStreamConfigFromURI(uri)
	if err != nil {
		t.Fatalf("kafkaStreamConfigFromURI returned error: %v", err)
	}
	if !reflect.DeepEqual(config.Brokers, []string{"broker.example.com:9092"}) {
		t.Fatalf("brokers = %#v, want default Kafka port", config.Brokers)
	}
	if config.Topic != "tickets" {
		t.Fatalf("topic = %q, want tickets", config.Topic)
	}
	if config.StartOffset != kafka.LastOffset {
		t.Fatalf("start offset = %d, want last offset by default", config.StartOffset)
	}
}

func TestKafkaStreamConfigFromURIUsesQueryBrokersWithTopicHost(t *testing.T) {
	uri, err := url.Parse("kafka://tickets?brokers=broker-a:9092,broker-b:9092")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	config, err := kafkaStreamConfigFromURI(uri)
	if err != nil {
		t.Fatalf("kafkaStreamConfigFromURI returned error: %v", err)
	}
	if !reflect.DeepEqual(config.Brokers, []string{"broker-a:9092", "broker-b:9092"}) {
		t.Fatalf("brokers = %#v, want query brokers", config.Brokers)
	}
	if config.Topic != "tickets" {
		t.Fatalf("topic = %q, want tickets", config.Topic)
	}
}

func TestKafkaStreamConfigFromURIUsesHostBrokerWithQueryTopic(t *testing.T) {
	uri, err := url.Parse("kafka://broker.example.com:9092?topic=tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	config, err := kafkaStreamConfigFromURI(uri)
	if err != nil {
		t.Fatalf("kafkaStreamConfigFromURI returned error: %v", err)
	}
	if !reflect.DeepEqual(config.Brokers, []string{"broker.example.com:9092"}) {
		t.Fatalf("brokers = %#v, want URI host broker", config.Brokers)
	}
	if config.Topic != "tickets" {
		t.Fatalf("topic = %q, want tickets", config.Topic)
	}
}

func TestKafkaStreamConfigRequiresBrokers(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "")
	uri, err := url.Parse("kafka://tickets")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	_, err = kafkaStreamConfigFromURI(uri)
	if err == nil || !strings.Contains(err.Error(), "brokers are required") {
		t.Fatalf("kafkaStreamConfigFromURI error = %v, want broker requirement", err)
	}
}

func TestReceiveKafkaStreamAdaptsMessageMetadata(t *testing.T) {
	reader := &fakeKafkaReader{messages: []kafka.Message{{
		Topic:     "tickets",
		Partition: 3,
		Offset:    42,
		Key:       []byte("tenant-a"),
		Value:     []byte(`{"event":"created"}`),
	}}}

	message, err := receiveKafkaStream(context.Background(), reader, false)
	if err != nil {
		t.Fatalf("receiveKafkaStream returned error: %v", err)
	}
	if message.ID != "tickets:3:42" {
		t.Fatalf("id = %q, want topic:partition:offset", message.ID)
	}
	if message.Body != `{"event":"created"}` {
		t.Fatalf("body = %q, want Kafka value", message.Body)
	}
	if message.Metadata["topic"] != "tickets" ||
		message.Metadata["partition"] != "3" ||
		message.Metadata["offset"] != "42" ||
		message.Metadata["key"] != "tenant-a" {
		t.Fatalf("metadata = %#v, want Kafka metadata", message.Metadata)
	}
}

func TestReceiveKafkaStreamCommitsMessageWhenAcknowledged(t *testing.T) {
	reader := &fakeKafkaReader{messages: []kafka.Message{{
		Topic:     "tickets",
		Partition: 1,
		Offset:    9,
		Value:     []byte(`{"event":"created"}`),
	}}}

	message, err := receiveKafkaStream(context.Background(), reader, true)
	if err != nil {
		t.Fatalf("receiveKafkaStream returned error: %v", err)
	}
	if err := ackStreamMessage(context.Background(), message); err != nil {
		t.Fatalf("ackStreamMessage returned error: %v", err)
	}
	if len(reader.committed) != 1 || reader.committed[0].Offset != 9 {
		t.Fatalf("committed messages = %+v, want Kafka offset 9", reader.committed)
	}
}

func TestDefaultStreamReceiverReusesKafkaReaderPerURIAndClosesIt(t *testing.T) {
	t.Setenv("KAFKA_BROKERS", "broker-a:9092")
	created := 0
	reader := &fakeKafkaReader{messages: []kafka.Message{
		{Topic: "tickets", Partition: 0, Offset: 1, Value: []byte(`{"n":1}`)},
		{Topic: "tickets", Partition: 0, Offset: 2, Value: []byte(`{"n":2}`)},
	}}
	receiver := newDefaultStreamReceiver()
	receiver.kafkaNewReader = func(config kafkaStreamConfig) (kafkaStreamReader, error) {
		created++
		if config.Topic != "tickets" {
			t.Fatalf("topic = %q, want tickets", config.Topic)
		}
		return reader, nil
	}

	first, err := receiver.Receive(context.Background(), "kafka://tickets")
	if err != nil {
		t.Fatalf("first Receive returned error: %v", err)
	}
	second, err := receiver.Receive(context.Background(), "kafka://tickets")
	if err != nil {
		t.Fatalf("second Receive returned error: %v", err)
	}
	if first.ID != "tickets:0:1" || second.ID != "tickets:0:2" {
		t.Fatalf("ids = %q/%q, want sequential Kafka offsets", first.ID, second.ID)
	}
	if created != 1 {
		t.Fatalf("created readers = %d, want one reusable reader", created)
	}
	if err := receiver.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if !reader.closed {
		t.Fatal("Kafka reader was not closed")
	}
}

type fakeKafkaReader struct {
	messages  []kafka.Message
	index     int
	closed    bool
	committed []kafka.Message
}

func (r *fakeKafkaReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if err := ctx.Err(); err != nil {
		return kafka.Message{}, err
	}
	if r.index >= len(r.messages) {
		return kafka.Message{}, errors.New("no message")
	}
	message := r.messages[r.index]
	r.index++
	return message, nil
}

func (r *fakeKafkaReader) Close() error {
	r.closed = true
	return nil
}

func (r *fakeKafkaReader) CommitMessages(ctx context.Context, messages ...kafka.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.committed = append(r.committed, messages...)
	return nil
}
