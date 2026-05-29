package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// OTLPHTTPClient is the minimal HTTP surface the OTLP exporter depends on. The
// standard *http.Client satisfies it; tests inject a fake to capture exported
// payloads without a live collector.
type OTLPHTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// OTLPExporter is a Tracer that ships spans to an OTLP/HTTP collector using the
// JSON encoding (Content-Type application/json) of the OpenTelemetry trace
// service. It is hand-rolled (no otel SDK dependency) and posts one span per
// completed operation to <endpoint>/v1/traces.
//
// Span attributes are sanitized through the package redaction helpers before
// export so secrets never reach the collector. Export errors are swallowed:
// observability must never break the pipeline.
type OTLPExporter struct {
	endpoint    string
	client      OTLPHTTPClient
	serviceName string
	headers     map[string]string
	now         func() time.Time
}

// OTLPOption configures an OTLPExporter.
type OTLPOption func(*OTLPExporter)

// WithOTLPHTTPClient injects the HTTP client used to post spans. Defaults to a
// client with a short timeout.
func WithOTLPHTTPClient(client OTLPHTTPClient) OTLPOption {
	return func(e *OTLPExporter) {
		if client != nil {
			e.client = client
		}
	}
}

// WithOTLPServiceName sets the service.name resource attribute.
func WithOTLPServiceName(name string) OTLPOption {
	return func(e *OTLPExporter) {
		name = strings.TrimSpace(name)
		if name != "" {
			e.serviceName = name
		}
	}
}

// WithOTLPHeaders sets additional headers (e.g. authorization for hosted
// collectors) attached to every export request.
func WithOTLPHeaders(headers map[string]string) OTLPOption {
	return func(e *OTLPExporter) {
		if len(headers) == 0 {
			return
		}
		if e.headers == nil {
			e.headers = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			k = strings.TrimSpace(k)
			if k != "" {
				e.headers[k] = v
			}
		}
	}
}

// NewOTLPExporter builds an OTLPExporter that posts spans to endpoint. The
// endpoint is the collector base URL (e.g. "https://collector:4318"); the
// exporter appends the "/v1/traces" path. A trailing "/v1/traces" on the
// endpoint is accepted as-is.
func NewOTLPExporter(endpoint string, opts ...OTLPOption) (*OTLPExporter, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("otlp exporter endpoint is required")
	}
	exporter := &OTLPExporter{
		endpoint:    otlpTracesURL(endpoint),
		client:      &http.Client{Timeout: 10 * time.Second},
		serviceName: "ouvrier",
		now:         time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(exporter)
		}
	}
	return exporter, nil
}

func otlpTracesURL(endpoint string) string {
	trimmed := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(trimmed, "/v1/traces") {
		return trimmed
	}
	return trimmed + "/v1/traces"
}

// StartSpan implements the Tracer interface. It returns a span that records its
// attributes and posts an OTLP payload when End is called.
func (e *OTLPExporter) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span) {
	span := &otlpSpan{
		exporter:   e,
		name:       name,
		start:      e.now().UTC(),
		traceID:    otlpTraceIDFromAttrs(attrs),
		spanID:     randomHex(8),
		attrs:      make(map[string]any),
		statusCode: otlpStatusUnset,
	}
	for k, v := range attrs {
		span.attrs[k] = v
	}
	return ctx, span
}

type otlpSpan struct {
	exporter   *OTLPExporter
	name       string
	start      time.Time
	traceID    string
	spanID     string
	attrs      map[string]any
	statusCode int
	statusMsg  string
	once       sync.Once
}

func (s *otlpSpan) SetAttribute(key string, value any) {
	if s == nil || key == "" {
		return
	}
	s.attrs[key] = value
	if key == "trace_id" {
		if id, ok := value.(string); ok {
			if normalized := otlpTraceID(id); normalized != "" {
				s.traceID = normalized
			}
		}
	}
}

func (s *otlpSpan) RecordError(err error) {
	if s == nil {
		return
	}
	s.statusCode = otlpStatusError
	if err != nil {
		s.statusMsg = err.Error()
	}
}

func (s *otlpSpan) End() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		end := s.exporter.now().UTC()
		s.exporter.export(s, end)
	})
}

func (e *OTLPExporter) export(span *otlpSpan, end time.Time) {
	if e == nil || e.client == nil {
		return
	}
	payload := e.payload(span, end)
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func (e *OTLPExporter) payload(span *otlpSpan, end time.Time) otlpTracePayload {
	traceID := span.traceID
	if traceID == "" {
		traceID = randomHex(16)
	}
	attrs := make([]otlpKeyValue, 0, len(span.attrs))
	for _, key := range sortedKeys(span.attrs) {
		value := span.attrs[key]
		if isSensitivePayloadKey(key) {
			attrs = append(attrs, otlpStringAttribute(key, redactedPayloadValue))
			continue
		}
		attrs = append(attrs, otlpAttribute(key, value))
	}
	status := otlpStatus{Code: span.statusCode}
	if span.statusMsg != "" {
		status.Message = RedactText(span.statusMsg)
	}
	return otlpTracePayload{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{otlpStringAttribute("service.name", e.serviceName)},
			},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: "github.com/ArnaudGuiovanna/ouvrier"},
				Spans: []otlpSpanData{{
					TraceID:           traceID,
					SpanID:            span.spanID,
					Name:              span.name,
					Kind:              otlpSpanKindInternal,
					StartTimeUnixNano: uint64(span.start.UnixNano()),
					EndTimeUnixNano:   uint64(end.UnixNano()),
					Attributes:        attrs,
					Status:            status,
				}},
			}},
		}},
	}
}

const (
	otlpStatusUnset = 0
	otlpStatusError = 2

	otlpSpanKindInternal = 1
)

type otlpTracePayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope      `json:"scope"`
	Spans []otlpSpanData `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpanData struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano uint64         `json:"startTimeUnixNano"`
	EndTimeUnixNano   uint64         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpAnyValue struct {
	StringValue string   `json:"stringValue,omitempty"`
	IntValue    *int64   `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

func otlpStringAttribute(key, value string) otlpKeyValue {
	return otlpKeyValue{Key: key, Value: otlpAnyValue{StringValue: value}}
}

func otlpAttribute(key string, value any) otlpKeyValue {
	switch typed := value.(type) {
	case string:
		return otlpStringAttribute(key, RedactText(typed))
	case bool:
		b := typed
		return otlpKeyValue{Key: key, Value: otlpAnyValue{BoolValue: &b}}
	case int:
		return otlpIntAttribute(key, int64(typed))
	case int8:
		return otlpIntAttribute(key, int64(typed))
	case int16:
		return otlpIntAttribute(key, int64(typed))
	case int32:
		return otlpIntAttribute(key, int64(typed))
	case int64:
		return otlpIntAttribute(key, typed)
	case uint:
		return otlpIntAttribute(key, int64(typed))
	case uint8:
		return otlpIntAttribute(key, int64(typed))
	case uint16:
		return otlpIntAttribute(key, int64(typed))
	case uint32:
		return otlpIntAttribute(key, int64(typed))
	case uint64:
		return otlpIntAttribute(key, int64(typed))
	case float32:
		return otlpDoubleAttribute(key, float64(typed))
	case float64:
		return otlpDoubleAttribute(key, typed)
	default:
		// Fall back to a sanitized JSON string so we never leak structured
		// secrets and always emit a valid OTLP value.
		encoded, err := json.Marshal(value)
		if err != nil {
			return otlpStringAttribute(key, "")
		}
		return otlpStringAttribute(key, RedactJSONText(string(encoded)))
	}
}

func otlpIntAttribute(key string, value int64) otlpKeyValue {
	v := value
	return otlpKeyValue{Key: key, Value: otlpAnyValue{IntValue: &v}}
}

func otlpDoubleAttribute(key string, value float64) otlpKeyValue {
	v := value
	return otlpKeyValue{Key: key, Value: otlpAnyValue{DoubleValue: &v}}
}

func otlpTraceIDFromAttrs(attrs map[string]any) string {
	if attrs == nil {
		return ""
	}
	if raw, ok := attrs["trace_id"].(string); ok {
		return otlpTraceID(raw)
	}
	return ""
}

// otlpTraceID normalizes an arbitrary trace identifier into a 32 hex-char
// (16-byte) OTLP trace id. A 32 hex-char input is used directly; anything else
// is hashed deterministically so the same TraceID maps to the same OTLP id.
func otlpTraceID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) == 32 && isHex(raw) {
		return strings.ToLower(raw)
	}
	sum := deterministicHash16(raw)
	return hex.EncodeToString(sum)
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func deterministicHash16(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:16]
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
