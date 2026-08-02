package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeBoundedProviderJSONRejectsOversizedAndTrailingBodies(t *testing.T) {
	var decoded map[string]any
	oversized := strings.NewReader(strings.Repeat(" ", maxProviderJSONResponseBytes+1))
	if err := decodeBoundedProviderJSON(oversized, &decoded); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized decode error = %v", err)
	}
	if err := decodeBoundedProviderJSON(strings.NewReader(`{} {}`), &decoded); err == nil {
		t.Fatal("multiple JSON values were accepted as one provider response")
	}
}

func TestScanSSEBoundsOneMultilineFrame(t *testing.T) {
	line := "data: " + strings.Repeat("x", 64*1024) + "\n"
	stream := strings.NewReader(strings.Repeat(line, maxProviderSSEFrameBytes/(64*1024)+1) + "\n")
	err := scanSSE(stream, func(sseEvent) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "SSE frame exceeds") {
		t.Fatalf("scanSSE() error = %v", err)
	}
}

func TestStreamingDecodersRejectMalformedEvents(t *testing.T) {
	malformed := "data: {not-json}\n\n"
	if _, err := decodeAnthropicStream(strings.NewReader(malformed), nil); err == nil || !strings.Contains(err.Error(), "stream event") {
		t.Fatalf("decodeAnthropicStream() error = %v", err)
	}
	if _, err := decodeOpenAICompatStream(strings.NewReader(malformed), nil); err == nil || !strings.Contains(err.Error(), "stream event") {
		t.Fatalf("decodeOpenAICompatStream() error = %v", err)
	}
}

func TestOpenAIStreamBoundsCumulativeTextBeforeCallback(t *testing.T) {
	chunk := strings.Repeat("x", 512*1024)
	payload, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": chunk}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stream strings.Builder
	for i := 0; i < maxProviderTextBytes/len(chunk)+1; i++ {
		stream.WriteString("data: ")
		stream.Write(payload)
		stream.WriteString("\n\n")
	}
	callbackBytes := 0
	_, err = decodeOpenAICompatStream(strings.NewReader(stream.String()), func(delta Delta) {
		callbackBytes += len(delta.Text)
	})
	if err == nil || !strings.Contains(err.Error(), "response text exceeds") {
		t.Fatalf("decodeOpenAICompatStream() error = %v", err)
	}
	if callbackBytes > maxProviderTextBytes {
		t.Fatalf("callback received %d bytes, limit %d", callbackBytes, maxProviderTextBytes)
	}
}

func TestProviderToolArgumentAndIdentityBounds(t *testing.T) {
	if err := providerToolArgsOverflow(maxProviderToolArgsBytes, 1); err == nil {
		t.Fatal("tool argument overflow was accepted")
	}
	if err := validateProviderToolIdentity(strings.Repeat("i", maxProviderToolIdentityBytes+1), "tool"); err == nil {
		t.Fatal("oversized tool call ID was accepted")
	}
	if err := validateProviderToolIdentity("id", strings.Repeat("n", maxProviderToolIdentityBytes+1)); err == nil {
		t.Fatal("oversized tool name was accepted")
	}
}
