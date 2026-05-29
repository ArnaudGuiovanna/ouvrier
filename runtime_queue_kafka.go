package ovr

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/segmentio/kafka-go"
)

// kafkaQueueWriter is the minimal writer boundary used by the Kafka push
// terminal so tests can inject a fake writer instead of a live broker.
type kafkaQueueWriter interface {
	WriteMessages(context.Context, ...kafka.Message) error
	Close() error
}

type kafkaQueuePublishConfig struct {
	Brokers []string
	Topic   string
}

// kafkaQueueWriterFactory builds a writer for the given config. It is a package
// variable so tests can swap in a fake; production uses newKafkaQueueWriter.
var kafkaQueueWriterFactory = newKafkaQueueWriter

func publishKafkaQueue(ctx context.Context, uri *url.URL, output string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	config, err := kafkaQueuePublishConfigFromURI(uri)
	if err != nil {
		return err
	}
	writer, err := kafkaQueueWriterFactory(config)
	if err != nil {
		return err
	}
	defer writer.Close()

	message := kafka.Message{Value: []byte(output)}
	if key := queueIdempotencyKey(uri); key != "" {
		message.Key = []byte(key)
	}
	return writer.WriteMessages(ctx, message)
}

func newKafkaQueueWriter(config kafkaQueuePublishConfig) (kafkaQueueWriter, error) {
	return &kafka.Writer{
		Addr:     kafka.TCP(config.Brokers...),
		Topic:    config.Topic,
		Balancer: &kafka.LeastBytes{},
	}, nil
}

func kafkaQueuePublishConfigFromURI(uri *url.URL) (kafkaQueuePublishConfig, error) {
	if uri == nil {
		return kafkaQueuePublishConfig{}, fmt.Errorf("kafka queue URI is required")
	}
	topic := strings.Trim(strings.TrimSpace(uri.Path), "/")
	if queryTopic := strings.TrimSpace(uri.Query().Get("topic")); queryTopic != "" {
		topic = queryTopic
	}
	brokers := kafkaBrokersFromURI(uri)
	if topic == "" && uri.Host != "" && !strings.Contains(uri.Host, ":") {
		topic = strings.TrimSpace(uri.Host)
	}
	if topic == "" {
		return kafkaQueuePublishConfig{}, fmt.Errorf("kafka queue topic is required")
	}
	if strings.ContainsAny(topic, " \t\r\n") {
		return kafkaQueuePublishConfig{}, fmt.Errorf("kafka queue topic is invalid")
	}
	if len(brokers) == 0 {
		brokers = kafkaBrokersFromEnv()
	}
	if len(brokers) == 0 {
		return kafkaQueuePublishConfig{}, fmt.Errorf("kafka queue brokers are required")
	}
	return kafkaQueuePublishConfig{Brokers: brokers, Topic: topic}, nil
}
