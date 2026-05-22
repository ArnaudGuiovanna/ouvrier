package ovr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/segmentio/kafka-go"
)

const defaultKafkaMaxBytes = 10 << 20

type kafkaStreamReaderFactory func(kafkaStreamConfig) (kafkaStreamReader, error)

type kafkaStreamReader interface {
	FetchMessage(context.Context) (kafka.Message, error)
	CommitMessages(context.Context, ...kafka.Message) error
	Close() error
}

type kafkaStreamConsumer struct {
	reader kafkaStreamReader
	commit bool
}

type kafkaStreamConfig struct {
	Brokers     []string
	Topic       string
	GroupID     string
	StartOffset int64
}

func (r *defaultStreamReceiver) receiveKafka(ctx context.Context, rawURI string, uri *url.URL) (streamMessage, error) {
	consumer, err := r.kafkaReader(rawURI, uri)
	if err != nil {
		return streamMessage{}, err
	}
	return receiveKafkaStream(ctx, consumer.reader, consumer.commit)
}

func (r *defaultStreamReceiver) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var closeErr error
	for rawURI, reader := range r.kafkaReaders {
		if reader != nil {
			closeErr = errors.Join(closeErr, reader.Close())
		}
		delete(r.kafkaReaders, rawURI)
		delete(r.kafkaCommit, rawURI)
	}
	return closeErr
}

func (r *defaultStreamReceiver) kafkaReader(rawURI string, uri *url.URL) (kafkaStreamConsumer, error) {
	if r == nil {
		config, err := kafkaStreamConfigFromURI(uri)
		if err != nil {
			return kafkaStreamConsumer{}, err
		}
		reader, err := newKafkaStreamReader(config)
		return kafkaStreamConsumer{reader: reader, commit: config.GroupID != ""}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.kafkaReaders == nil {
		r.kafkaReaders = make(map[string]kafkaStreamReader)
	}
	if r.kafkaCommit == nil {
		r.kafkaCommit = make(map[string]bool)
	}
	if reader := r.kafkaReaders[rawURI]; reader != nil {
		return kafkaStreamConsumer{reader: reader, commit: r.kafkaShouldCommit(rawURI)}, nil
	}

	config, err := kafkaStreamConfigFromURI(uri)
	if err != nil {
		return kafkaStreamConsumer{}, err
	}
	factory := r.kafkaNewReader
	if factory == nil {
		factory = newKafkaStreamReader
	}
	reader, err := factory(config)
	if err != nil {
		return kafkaStreamConsumer{}, err
	}
	r.kafkaReaders[rawURI] = reader
	r.rememberKafkaCommitMode(rawURI, config.GroupID != "")
	return kafkaStreamConsumer{reader: reader, commit: config.GroupID != ""}, nil
}

func (r *defaultStreamReceiver) kafkaShouldCommit(rawURI string) bool {
	return r != nil && r.kafkaCommit != nil && r.kafkaCommit[rawURI]
}

func (r *defaultStreamReceiver) rememberKafkaCommitMode(rawURI string, commit bool) {
	if r == nil {
		return
	}
	if r.kafkaCommit == nil {
		r.kafkaCommit = make(map[string]bool)
	}
	r.kafkaCommit[rawURI] = commit
}

func newKafkaStreamReader(config kafkaStreamConfig) (kafkaStreamReader, error) {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:     append([]string(nil), config.Brokers...),
		Topic:       config.Topic,
		GroupID:     config.GroupID,
		StartOffset: config.StartOffset,
		MinBytes:    1,
		MaxBytes:    defaultKafkaMaxBytes,
	}), nil
}

func receiveKafkaStream(ctx context.Context, reader kafkaStreamReader, commit bool) (streamMessage, error) {
	if reader == nil {
		return streamMessage{}, fmt.Errorf("kafka stream reader is required")
	}
	message, err := reader.FetchMessage(ctx)
	if err != nil {
		return streamMessage{}, err
	}
	streamMessage := kafkaStreamMessage(message)
	if commit {
		streamMessage.ack = func(ctx context.Context) error {
			return reader.CommitMessages(ctx, message)
		}
	}
	return streamMessage, nil
}

func kafkaStreamMessage(message kafka.Message) streamMessage {
	metadata := map[string]string{
		"topic":     message.Topic,
		"partition": strconv.Itoa(message.Partition),
		"offset":    strconv.FormatInt(message.Offset, 10),
	}
	if len(message.Key) > 0 {
		metadata["key"] = string(message.Key)
	}
	return streamMessage{
		ID:       strings.Join([]string{message.Topic, strconv.Itoa(message.Partition), strconv.FormatInt(message.Offset, 10)}, ":"),
		Body:     string(message.Value),
		Metadata: metadata,
	}
}

func kafkaStreamConfigFromURI(uri *url.URL) (kafkaStreamConfig, error) {
	if uri == nil {
		return kafkaStreamConfig{}, fmt.Errorf("kafka stream URI is required")
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
		return kafkaStreamConfig{}, fmt.Errorf("kafka stream topic is required")
	}
	if strings.ContainsAny(topic, " \t\r\n") {
		return kafkaStreamConfig{}, fmt.Errorf("kafka stream topic is invalid")
	}
	if len(brokers) == 0 {
		brokers = kafkaBrokersFromEnv()
	}
	if len(brokers) == 0 {
		return kafkaStreamConfig{}, fmt.Errorf("kafka stream brokers are required")
	}

	startOffset, err := kafkaStartOffset(uri.Query().Get("start"))
	if err != nil {
		return kafkaStreamConfig{}, err
	}
	return kafkaStreamConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     kafkaGroupID(uri),
		StartOffset: startOffset,
	}, nil
}

func kafkaBrokersFromURI(uri *url.URL) []string {
	if raw := strings.TrimSpace(uri.Query().Get("brokers")); raw != "" {
		return cleanCSV(raw)
	}
	pathTopic := strings.Trim(strings.TrimSpace(uri.Path), "/")
	queryTopic := strings.TrimSpace(uri.Query().Get("topic"))
	if (pathTopic != "" || queryTopic != "") && uri.Host != "" {
		return []string{kafkaBrokerAddress(uri.Host)}
	}
	return nil
}

func kafkaBrokersFromEnv() []string {
	return cleanCSV(os.Getenv("KAFKA_BROKERS"))
}

func kafkaBrokerAddress(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		return host
	}
	return net.JoinHostPort(host, "9092")
}

func kafkaGroupID(uri *url.URL) string {
	if value := strings.TrimSpace(uri.Query().Get("group")); value != "" {
		return value
	}
	return strings.TrimSpace(uri.Query().Get("group_id"))
}

func kafkaStartOffset(value string) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "last", "latest", "new":
		return kafka.LastOffset, nil
	case "first", "earliest", "oldest":
		return kafka.FirstOffset, nil
	default:
		return 0, fmt.Errorf("kafka stream start offset must be first or last")
	}
}

func cleanCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return cleaned
}
