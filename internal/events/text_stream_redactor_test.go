package events

import (
	"strings"
	"testing"
)

func TestTextStreamRedactorMasksSecretsAcrossArbitraryChunks(t *testing.T) {
	const configured = "configured-secret-value-12345"
	t.Setenv("WORKER_API_KEY", configured)

	tests := []struct {
		name   string
		chunks []string
	}{
		{name: "bearer", chunks: []string{"prefix Bea", "rer bearer-secret-value", " suffix"}},
		{name: "authorization bearer", chunks: []string{"Authorization: Bea", "rer authorization-secret-value", "\nnext"}},
		{name: "json assignment", chunks: []string{`{"api_`, `key":"json-secret-`, `value"}`}},
		{name: "known token", chunks: []string{"token sk-12345", "67890abcdefghijk tail"}},
		{name: "configured value", chunks: []string{"value " + configured[:9], configured[9:18], configured[18:] + " end"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			redactor := NewTextStreamRedactor()
			var output strings.Builder
			for _, chunk := range test.chunks {
				output.WriteString(redactor.Push(chunk))
				if len(redactor.pending) > maxTextStreamPendingBytes {
					t.Fatalf("pending bytes = %d, want <= %d", len(redactor.pending), maxTextStreamPendingBytes)
				}
			}
			output.WriteString(redactor.Flush())
			got := output.String()
			for _, secret := range []string{"bearer-secret-value", "authorization-secret-value", "json-secret-value", "sk-1234567890abcdefghijk", configured} {
				if strings.Contains(got, secret) {
					t.Fatalf("stream output leaked %q: %q", secret, got)
				}
			}
			if !strings.Contains(got, redactedPayloadValue) {
				t.Fatalf("stream output = %q, want redaction marker", got)
			}
		})
	}
}

func TestTextStreamRedactorBoundsUnterminatedSecretState(t *testing.T) {
	redactor := NewTextStreamRedactor()
	var output strings.Builder
	output.WriteString(redactor.Push(`{"password":"`))
	for range 64 {
		output.WriteString(redactor.Push(strings.Repeat("x", 1024)))
		if len(redactor.pending) > maxTextStreamPendingBytes {
			t.Fatalf("pending bytes = %d, want <= %d", len(redactor.pending), maxTextStreamPendingBytes)
		}
	}
	output.WriteString(redactor.Flush())
	if strings.Contains(output.String(), strings.Repeat("x", 32)) {
		t.Fatalf("unterminated secret content leaked: %.100q", output.String())
	}
	if !strings.Contains(output.String(), redactedPayloadValue) {
		t.Fatalf("output = %q, want redaction marker", output.String())
	}
}
