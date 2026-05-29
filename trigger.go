package ovr

import (
	"fmt"
	"net/url"
	"strings"
)

type triggerSource interface {
	validateTrigger() error
}

type httpTrigger struct {
	method string
	path   string
}

func (t httpTrigger) validateTrigger() error {
	if t.method == "" || t.path == "" {
		return fmt.Errorf("%w: HTTP trigger requires method and path", ErrInvalidNode)
	}
	return nil
}

// CronTrigger is a cron expression trigger source.
type CronTrigger struct {
	expr string
}

// Cron declares a cron trigger source for From.
func Cron(expr string) CronTrigger {
	return CronTrigger{expr: strings.TrimSpace(expr)}
}

func (t CronTrigger) validateTrigger() error {
	if t.expr == "" {
		return fmt.Errorf("%w: cron expression is required", ErrInvalidNode)
	}
	if _, err := parseCronSchedule(t.expr); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	return nil
}

// WebhookEndpoint can be used as a signed webhook trigger or a push target.
type WebhookEndpoint struct {
	value string
}

// Webhook declares a webhook trigger source or push target.
func Webhook(value string) WebhookEndpoint {
	return WebhookEndpoint{value: strings.TrimSpace(value)}
}

func (t WebhookEndpoint) validateTrigger() error {
	if t.value == "" {
		return fmt.Errorf("%w: webhook provider is required", ErrInvalidNode)
	}
	if !validWebhookProviderName(t.value) {
		return fmt.Errorf("%w: webhook provider must contain only letters, digits, dot, dash, or underscore", ErrInvalidNode)
	}
	return nil
}

func (t WebhookEndpoint) validatePushTarget() error {
	if t.value == "" {
		return fmt.Errorf("%w: webhook URL is required", ErrInvalidNode)
	}
	return nil
}

func (t WebhookEndpoint) pushWebhookURL() string {
	return t.value
}

func validWebhookProviderName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// StreamTrigger is a stream trigger source.
type StreamTrigger struct {
	uri string
}

// Stream declares a stream trigger source for From.
func Stream(uri string) StreamTrigger {
	return StreamTrigger{uri: strings.TrimSpace(uri)}
}

func (t StreamTrigger) validateTrigger() error {
	if t.uri == "" {
		return fmt.Errorf("%w: stream URI is required", ErrInvalidNode)
	}
	if err := validateStreamURI(t.uri); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidNode, err)
	}
	return nil
}

func validateStreamURI(raw string) error {
	if strings.ContainsRune(raw, 0) {
		return fmt.Errorf("stream URI is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return fmt.Errorf("stream URI must include a supported scheme")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "kafka", "nats":
		if strings.Trim(strings.TrimSpace(parsed.Host+parsed.Path), "/") == "" {
			return fmt.Errorf("%s stream URI must include a topic or subject", parsed.Scheme)
		}
	case "redis":
		if parsed.Host == "" {
			return fmt.Errorf("redis stream URI must include a host")
		}
		if strings.Trim(strings.TrimSpace(parsed.Path), "/") == "" {
			return fmt.Errorf("redis stream URI must include a stream name")
		}
	default:
		return fmt.Errorf("unsupported stream scheme %q", parsed.Scheme)
	}
	return nil
}

// FromOption configures a From node.
type FromOption interface {
	applyFrom(*fromConfig)
}

type fromConfig struct {
	workerPool        int
	idempotencyHeader string
	signatureEnv      string
	signatureHeader   string
	dlqTarget         string
	maxAttempts       int
	maxInFlight       int
	err               error
}

type workerPoolOption struct {
	limit int
}

// WorkerPool bounds concurrent trigger executions for a pipeline.
func WorkerPool(limit int) FromOption {
	return workerPoolOption{limit: limit}
}

func (o workerPoolOption) applyFrom(config *fromConfig) {
	if o.limit <= 0 {
		config.setErr(fmt.Errorf("%w: WorkerPool must be greater than zero", ErrInvalidNode))
		return
	}
	config.workerPool = o.limit
}

type streamDLQOption struct {
	target      string
	maxAttempts int
}

// StreamDLQ routes a poisoned stream message to a dead-letter target once it
// has been redelivered maxAttempts times without success. maxAttempts counts
// total processing attempts before the message is dead-lettered.
func StreamDLQ(target string, maxAttempts int) FromOption {
	return streamDLQOption{target: strings.TrimSpace(target), maxAttempts: maxAttempts}
}

func (o streamDLQOption) applyFrom(config *fromConfig) {
	if o.target == "" {
		config.setErr(fmt.Errorf("%w: StreamDLQ target is required", ErrInvalidNode))
		return
	}
	if err := validateStreamURI(o.target); err != nil {
		config.setErr(fmt.Errorf("%w: StreamDLQ target %v", ErrInvalidNode, err))
		return
	}
	if o.maxAttempts <= 0 {
		config.setErr(fmt.Errorf("%w: StreamDLQ maxAttempts must be greater than zero", ErrInvalidNode))
		return
	}
	config.dlqTarget = o.target
	config.maxAttempts = o.maxAttempts
}

type streamMaxInFlightOption struct {
	limit int
}

// StreamMaxInFlight bounds the number of stream messages processed
// concurrently so a slow handler applies backpressure to the broker instead of
// buffering deliveries without bound.
func StreamMaxInFlight(limit int) FromOption {
	return streamMaxInFlightOption{limit: limit}
}

func (o streamMaxInFlightOption) applyFrom(config *fromConfig) {
	if o.limit <= 0 {
		config.setErr(fmt.Errorf("%w: StreamMaxInFlight must be greater than zero", ErrInvalidNode))
		return
	}
	config.maxInFlight = o.limit
}

type idempotencyKeyOption struct {
	header string
}

// IdempotencyKey prevents duplicate trigger deliveries based on a request header.
func IdempotencyKey(header string) FromOption {
	return idempotencyKeyOption{header: strings.TrimSpace(header)}
}

func (o idempotencyKeyOption) applyFrom(config *fromConfig) {
	if o.header == "" {
		config.setErr(fmt.Errorf("%w: IdempotencyKey header is required", ErrInvalidNode))
		return
	}
	config.idempotencyHeader = o.header
}

type verifySignatureOption struct {
	envVar string
	header string
}

// VerifySignature verifies an HMAC-SHA256 request signature before executing a trigger.
func VerifySignature(envVar, header string) FromOption {
	return verifySignatureOption{
		envVar: strings.TrimSpace(envVar),
		header: strings.TrimSpace(header),
	}
}

func (o verifySignatureOption) applyFrom(config *fromConfig) {
	if o.envVar == "" {
		config.setErr(fmt.Errorf("%w: VerifySignature env var is required", ErrInvalidNode))
		return
	}
	if o.header == "" {
		config.setErr(fmt.Errorf("%w: VerifySignature header is required", ErrInvalidNode))
		return
	}
	config.signatureEnv = o.envVar
	config.signatureHeader = o.header
}

func (c *fromConfig) setErr(err error) {
	if c.err == nil {
		c.err = err
	}
}

type fromNode struct {
	source triggerSource
	err    error
	config fromConfig
}

// From declares the event source that starts a pipeline.
func From(source any, options ...FromOption) Node {
	node := fromNode{}
	switch source := source.(type) {
	case string:
		parsed, err := parseHTTPTrigger(source)
		node.source = parsed
		node.err = err
	case triggerSource:
		node.source = source
	default:
		node.err = fmt.Errorf("%w: unsupported From source %T", ErrInvalidNode, source)
	}

	for _, option := range options {
		if option == nil {
			node.err = fmt.Errorf("%w: nil From option", ErrInvalidNode)
			continue
		}
		option.applyFrom(&node.config)
	}
	return node
}

func (n fromNode) nodeKind() nodeKind {
	return nodeKindFrom
}

func (n fromNode) validateNode() error {
	if n.err != nil {
		return n.err
	}
	if n.config.err != nil {
		return n.config.err
	}
	if n.source == nil {
		return fmt.Errorf("%w: From source is required", ErrInvalidNode)
	}
	return n.source.validateTrigger()
}

func parseHTTPTrigger(route string) (httpTrigger, error) {
	fields := strings.Fields(route)
	if len(fields) != 2 {
		return httpTrigger{}, fmt.Errorf("%w: HTTP trigger must be METHOD /path", ErrInvalidNode)
	}

	method := strings.ToUpper(fields[0])
	switch method {
	case "GET", "POST":
	default:
		return httpTrigger{}, fmt.Errorf("%w: unsupported HTTP method %q", ErrInvalidNode, fields[0])
	}

	path := fields[1]
	if !strings.HasPrefix(path, "/") {
		return httpTrigger{}, fmt.Errorf("%w: HTTP trigger path must start with /", ErrInvalidNode)
	}

	return httpTrigger{method: method, path: path}, nil
}
