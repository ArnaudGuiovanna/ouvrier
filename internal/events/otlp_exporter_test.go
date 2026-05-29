package events

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
)

type recordingHTTPClient struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	err      error
	status   int
}

func (c *recordingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	var body []byte
	if req.Body != nil {
		buf := make([]byte, 0, 1024)
		tmp := make([]byte, 512)
		for {
			n, err := req.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		body = buf
		_ = req.Body.Close()
	}
	c.requests = append(c.requests, req)
	c.bodies = append(c.bodies, body)
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: http.NoBody, Header: make(http.Header)}, nil
}

func (c *recordingHTTPClient) snapshot() ([]*http.Request, [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*http.Request(nil), c.requests...), append([][]byte(nil), c.bodies...)
}

func TestOTLPExporterRejectsEmptyEndpoint(t *testing.T) {
	if _, err := NewOTLPExporter(""); err == nil {
		t.Fatal("NewOTLPExporter(\"\") returned nil error, want endpoint-required error")
	}
}

func TestOTLPExporterPostsWellFormedSpanPayload(t *testing.T) {
	client := &recordingHTTPClient{}
	exporter, err := NewOTLPExporter("https://collector.example.com:4318",
		WithOTLPHTTPClient(client),
		WithOTLPServiceName("ouvrier-test"),
	)
	if err != nil {
		t.Fatalf("NewOTLPExporter returned error: %v", err)
	}

	// Drive a full started -> completed pairing through the tracer subscriber.
	sub := TracerSubscriber(exporter)
	sub(context.Background(), Event{Kind: EventLLMCallStarted, TraceID: "trace-otlp", Payload: map[string]any{"call_id": "c1", "model": "anthropic/claude"}})
	sub(context.Background(), Event{Kind: EventLLMCallCompleted, TraceID: "trace-otlp", Payload: map[string]any{"call_id": "c1", "latency_ms": 42}})

	reqs, bodies := client.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("export requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if got := req.URL.String(); got != "https://collector.example.com:4318/v1/traces" {
		t.Fatalf("url = %s, want .../v1/traces", got)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var payload otlpTracePayload
	if err := json.Unmarshal(bodies[0], &payload); err != nil {
		t.Fatalf("unmarshal OTLP payload: %v", err)
	}
	if len(payload.ResourceSpans) != 1 {
		t.Fatalf("resource spans = %d, want 1", len(payload.ResourceSpans))
	}
	rs := payload.ResourceSpans[0]
	if !hasServiceName(rs.Resource.Attributes, "ouvrier-test") {
		t.Fatalf("resource attributes missing service.name=ouvrier-test: %+v", rs.Resource.Attributes)
	}
	if len(rs.ScopeSpans) != 1 || len(rs.ScopeSpans[0].Spans) != 1 {
		t.Fatalf("scope spans malformed: %+v", rs.ScopeSpans)
	}
	span := rs.ScopeSpans[0].Spans[0]
	if span.Name != "llm_call" {
		t.Fatalf("span name = %q, want llm_call", span.Name)
	}
	if span.TraceID == "" || len(span.TraceID) != 32 {
		t.Fatalf("trace id = %q, want 32 hex chars", span.TraceID)
	}
	if span.SpanID == "" || len(span.SpanID) != 16 {
		t.Fatalf("span id = %q, want 16 hex chars", span.SpanID)
	}
	if span.StartTimeUnixNano == 0 || span.EndTimeUnixNano == 0 {
		t.Fatalf("span times must be set: start=%d end=%d", span.StartTimeUnixNano, span.EndTimeUnixNano)
	}
	if span.EndTimeUnixNano < span.StartTimeUnixNano {
		t.Fatalf("end %d before start %d", span.EndTimeUnixNano, span.StartTimeUnixNano)
	}
	if !hasAttribute(span.Attributes, "latency_ms") {
		t.Fatalf("span attributes missing latency_ms: %+v", span.Attributes)
	}
}

func TestOTLPExporterMarksErrorStatusOnFailure(t *testing.T) {
	client := &recordingHTTPClient{}
	exporter, err := NewOTLPExporter("https://collector.example.com:4318", WithOTLPHTTPClient(client))
	if err != nil {
		t.Fatalf("NewOTLPExporter returned error: %v", err)
	}
	sub := TracerSubscriber(exporter)
	sub(context.Background(), Event{Kind: EventToolCallStarted, TraceID: "trace-err", Payload: map[string]any{"tool_call_id": "t1"}})
	sub(context.Background(), Event{Kind: EventToolCallFailed, TraceID: "trace-err", Payload: map[string]any{"tool_call_id": "t1", "error": "boom"}})

	_, bodies := client.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("export requests = %d, want 1", len(bodies))
	}
	var payload otlpTracePayload
	if err := json.Unmarshal(bodies[0], &payload); err != nil {
		t.Fatalf("unmarshal OTLP payload: %v", err)
	}
	span := payload.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if span.Status.Code != otlpStatusError {
		t.Fatalf("status code = %d, want %d (ERROR)", span.Status.Code, otlpStatusError)
	}
}

func TestOTLPExporterRedactsSensitiveAttributes(t *testing.T) {
	client := &recordingHTTPClient{}
	exporter, err := NewOTLPExporter("https://collector.example.com:4318", WithOTLPHTTPClient(client))
	if err != nil {
		t.Fatalf("NewOTLPExporter returned error: %v", err)
	}
	sub := TracerSubscriber(exporter)
	sub(context.Background(), Event{Kind: EventLLMCallStarted, TraceID: "trace-secret", Payload: map[string]any{"call_id": "c1", "api_key": "sk-secret-value"}})
	sub(context.Background(), Event{Kind: EventLLMCallCompleted, TraceID: "trace-secret", Payload: map[string]any{"call_id": "c1"}})

	_, bodies := client.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("export requests = %d, want 1", len(bodies))
	}
	if got := string(bodies[0]); got == "" {
		t.Fatal("empty export body")
	} else if containsRaw(got, "sk-secret-value") {
		t.Fatalf("export body leaked secret: %s", got)
	}
}

func TestOTLPExporterDoErrorDoesNotPanic(t *testing.T) {
	client := &recordingHTTPClient{err: errors.New("network down")}
	exporter, err := NewOTLPExporter("https://collector.example.com:4318", WithOTLPHTTPClient(client))
	if err != nil {
		t.Fatalf("NewOTLPExporter returned error: %v", err)
	}
	sub := TracerSubscriber(exporter)
	sub(context.Background(), Event{Kind: EventPipelineStarted, TraceID: "trace-x", SessionID: "s1"})
	sub(context.Background(), Event{Kind: EventPipelineCompleted, TraceID: "trace-x", SessionID: "s1"})
	// No assertion beyond "did not panic"; export errors must be swallowed.
}

func hasServiceName(attrs []otlpKeyValue, want string) bool {
	for _, a := range attrs {
		if a.Key == "service.name" && a.Value.StringValue == want {
			return true
		}
	}
	return false
}

func hasAttribute(attrs []otlpKeyValue, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

func containsRaw(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
