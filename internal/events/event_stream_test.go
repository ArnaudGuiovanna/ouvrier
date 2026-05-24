package events

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEventStreamAppendsEventsWithMonotonicIDs(t *testing.T) {
	now := time.Date(2026, 5, 18, 13, 0, 0, 0, time.UTC)
	stream, err := NewEventStream(WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	first, err := stream.Append(context.Background(), Event{
		Kind:      EventSessionStart,
		ExecID:    "exec_1",
		SessionID: "sess_1",
		Payload:   map[string]any{"model": "anthropic/claude-sonnet-4-6"},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	second, err := stream.Append(context.Background(), Event{Kind: EventAfterLLM})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	if first.ID != 1 || second.ID != 2 {
		t.Fatalf("IDs = %d, %d; want 1, 2", first.ID, second.ID)
	}
	if !first.At.Equal(now) {
		t.Fatalf("At = %v, want %v", first.At, now)
	}
	if first.Payload["model"] != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("Payload = %+v", first.Payload)
	}
}

func TestEventStreamReturnsDefensiveCopies(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	_, err = stream.Append(context.Background(), Event{
		Kind:    EventBeforeTool,
		Payload: map[string]any{"tool": "lookup"},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	events := stream.List()
	events[0].Kind = EventAfterTool
	events[0].Payload["tool"] = "mutated"

	again := stream.List()
	if again[0].Kind != EventBeforeTool {
		t.Fatalf("stored kind = %q, want %q", again[0].Kind, EventBeforeTool)
	}
	if again[0].Payload["tool"] != "lookup" {
		t.Fatalf("stored payload = %+v", again[0].Payload)
	}
}

func TestEventStreamAppendRedactsSensitivePayloadKeysRecursively(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	payload := map[string]any{
		"authorization": "Bearer root-token",
		"token":         "root-token",
		"api_key":       "root-api-key",
		"password":      "root-password",
		"secret":        "root-secret",
		"cookie":        "session=root-cookie",
		"safe":          "visible",
		"nested": map[string]any{
			"authorization": "Bearer nested-token",
			"token":         "nested-token",
			"api_key":       "nested-api-key",
			"password":      "nested-password",
			"secret":        "nested-secret",
			"cookie":        "session=nested-cookie",
			"safe":          "nested-visible",
		},
		"items": []any{
			map[string]any{
				"token": "slice-token",
				"safe":  "slice-visible",
			},
		},
	}

	appended, err := stream.Append(context.Background(), Event{
		Kind:    EventBeforeLLM,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	assertSensitivePayloadRedacted(t, appended.Payload)
	assertSensitivePayloadRedacted(t, stream.List()[0].Payload)
	if appended.Payload["safe"] != "visible" {
		t.Fatalf("safe payload field = %v, want visible", appended.Payload["safe"])
	}
	nested := appended.Payload["nested"].(map[string]any)
	if nested["safe"] != "nested-visible" {
		t.Fatalf("nested safe payload field = %v, want nested-visible", nested["safe"])
	}
	item := appended.Payload["items"].([]any)[0].(map[string]any)
	if item["safe"] != "slice-visible" {
		t.Fatalf("slice safe payload field = %v, want slice-visible", item["safe"])
	}
	if payload["token"] != "root-token" {
		t.Fatalf("Append mutated caller payload token = %v, want root-token", payload["token"])
	}
}

func TestEventStreamAppendRedactsSensitivePayloadStrings(t *testing.T) {
	t.Setenv("SERVICE_API_KEY", "sk-live-secret")
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	appended, err := stream.Append(context.Background(), Event{
		Kind: EventToolCallFailed,
		Payload: map[string]any{
			"error":  "request failed with api_key=sk-live-secret and Authorization: Bearer bearer-secret",
			"json":   `{"password":"json-secret","apiKey":"json-api-key","API_KEY":"json-api-upper","accessToken":"json-access-token","refreshToken":"json-refresh-token","clientSecret":"json-client-secret","secret_key":"json-secret-key","private_key":"json-private-key","safe":"visible"}`,
			"output": []byte("token=sk-live-secret access_token=access-secret secret_key=secret-key-value clientSecret=client-secret"),
		},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	for key, value := range appended.Payload {
		text := redactionTestString(value)
		if strings.Contains(text, "sk-live-secret") ||
			strings.Contains(text, "bearer-secret") ||
			strings.Contains(text, "json-secret") ||
			strings.Contains(text, "json-api-key") ||
			strings.Contains(text, "json-api-upper") ||
			strings.Contains(text, "json-access-token") ||
			strings.Contains(text, "json-refresh-token") ||
			strings.Contains(text, "json-client-secret") ||
			strings.Contains(text, "json-secret-key") ||
			strings.Contains(text, "json-private-key") ||
			strings.Contains(text, "access-secret") ||
			strings.Contains(text, "secret-key-value") ||
			strings.Contains(text, "client-secret") {
			t.Fatalf("%s payload leaked secret: %+v", key, appended.Payload)
		}
		if !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("%s payload = %q, want redacted value", key, text)
		}
	}
}

func TestEventStreamListReturnsDeepCopiesOfNestedPayloadValues(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	_, err = stream.Append(context.Background(), Event{
		Kind: EventBeforeTool,
		Payload: map[string]any{
			"metadata": map[string]any{
				"tool": "lookup",
				"tags": []any{"first", map[string]any{
					"label": "nested",
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	listed := stream.List()
	metadata := listed[0].Payload["metadata"].(map[string]any)
	metadata["tool"] = "mutated"
	tags := metadata["tags"].([]any)
	tags[0] = "mutated"
	nestedTag := tags[1].(map[string]any)
	nestedTag["label"] = "mutated"

	again := stream.List()
	want := map[string]any{
		"metadata": map[string]any{
			"tool": "lookup",
			"tags": []any{"first", map[string]any{
				"label": "nested",
			}},
		},
	}
	if !reflect.DeepEqual(again[0].Payload, want) {
		t.Fatalf("stored payload = %#v, want %#v", again[0].Payload, want)
	}
}

func TestEventStreamAppendHonorsCanceledContext(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = stream.Append(ctx, Event{Kind: EventSchemaViolation})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Append error = %v, want context.Canceled", err)
	}
	if len(stream.List()) != 0 {
		t.Fatalf("events = %d, want 0", len(stream.List()))
	}
}

func TestEventStreamAppendAcceptsNilContext(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	event, err := stream.Append(nil, Event{Kind: EventSessionStarted})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if event.ID != 1 {
		t.Fatalf("event ID = %d, want 1", event.ID)
	}
	if len(stream.List()) != 1 {
		t.Fatalf("events = %d, want 1", len(stream.List()))
	}
}

func TestEventStreamSinceFiltersByID(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	_, _ = stream.Append(context.Background(), Event{Kind: EventBeforeLLM})
	_, _ = stream.Append(context.Background(), Event{Kind: EventAfterLLM})
	_, _ = stream.Append(context.Background(), Event{Kind: EventSessionEnd})

	events := stream.Since(1)
	if len(events) != 2 {
		t.Fatalf("Since returned %d events, want 2", len(events))
	}
	if events[0].ID != 2 || events[1].ID != 3 {
		t.Fatalf("Since IDs = %d, %d; want 2, 3", events[0].ID, events[1].ID)
	}
}

func TestEventStreamSinceReturnsDefensiveCopiesOfHeaderPayloadValues(t *testing.T) {
	stream, err := NewEventStream()
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}
	headers := map[string][]string{
		"Authorization": {"Bearer secret-token"},
		"X-Request-ID":  {"req_1"},
	}
	_, err = stream.Append(context.Background(), Event{
		Kind:    EventBeforeTool,
		Payload: map[string]any{"headers": headers},
	})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}

	events := stream.Since(0)
	listedHeaders := events[0].Payload["headers"].(map[string][]string)
	if listedHeaders["Authorization"][0] != "[REDACTED]" {
		t.Fatalf("Authorization header = %v, want [REDACTED]", listedHeaders["Authorization"])
	}
	listedHeaders["Authorization"][0] = "mutated-secret"
	listedHeaders["X-Request-ID"][0] = "mutated"

	again := stream.Since(0)
	againHeaders := again[0].Payload["headers"].(map[string][]string)
	if againHeaders["Authorization"][0] != "[REDACTED]" {
		t.Fatalf("stored Authorization header = %v, want [REDACTED]", againHeaders["Authorization"])
	}
	if againHeaders["X-Request-ID"][0] != "req_1" {
		t.Fatalf("stored X-Request-ID header = %v, want req_1", againHeaders["X-Request-ID"])
	}
	if headers["Authorization"][0] != "Bearer secret-token" {
		t.Fatalf("Append mutated caller headers = %v, want original secret", headers["Authorization"])
	}
}

func TestEventStreamStartsAfterInitialID(t *testing.T) {
	stream, err := NewEventStream(WithInitialID(41))
	if err != nil {
		t.Fatalf("NewEventStream returned error: %v", err)
	}

	event, err := stream.Append(context.Background(), Event{Kind: EventSessionStarted})
	if err != nil {
		t.Fatalf("Append returned error: %v", err)
	}
	if event.ID != 42 {
		t.Fatalf("event ID = %d, want 42", event.ID)
	}
}

func assertSensitivePayloadRedacted(t *testing.T, payload map[string]any) {
	t.Helper()
	for _, key := range []string{"authorization", "token", "api_key", "password", "secret", "cookie"} {
		if payload[key] != "[REDACTED]" {
			t.Fatalf("payload[%q] = %v, want [REDACTED] in %+v", key, payload[key], payload)
		}
	}
	nested := payload["nested"].(map[string]any)
	for _, key := range []string{"authorization", "token", "api_key", "password", "secret", "cookie"} {
		if nested[key] != "[REDACTED]" {
			t.Fatalf("nested payload[%q] = %v, want [REDACTED] in %+v", key, nested[key], nested)
		}
	}
	item := payload["items"].([]any)[0].(map[string]any)
	if item["token"] != "[REDACTED]" {
		t.Fatalf("slice payload token = %v, want [REDACTED] in %+v", item["token"], item)
	}
}

func redactionTestString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}
