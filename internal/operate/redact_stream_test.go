package operate

import (
	"strings"
	"testing"
)

func TestRedactionStreamMasksStructuralSecretsAtEveryDeltaBoundary(t *testing.T) {
	tests := []struct {
		name     string
		redactor Redactor
		input    string
		secrets  []string
	}{
		{
			name:    "bearer",
			input:   "before Authorization: Bearer abcdefghijklmnop after",
			secrets: []string{"abcdefghijklmnop"},
		},
		{
			name:    "assignment",
			input:   `before OPENAI_API_KEY = "assignment-secret-value" after`,
			secrets: []string{"assignment-secret-value"},
		},
		{
			name:    "known token",
			input:   "before sk-abcdefghijklmnopqrstuvwx after",
			secrets: []string{"sk-abcdefghijklmnopqrstuvwx"},
		},
		{
			name:     "configured token",
			redactor: NewRedactor("configured-secret-value"),
			input:    "before configured-secret-value after",
			secrets:  []string{"configured-secret-value"},
		},
		{
			name:    "private key",
			input:   "before -----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY----- after",
			secrets: []string{"private-material", "-----BEGIN PRIVATE KEY-----"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := test.redactor.Redact(test.input)
			for cut := 1; cut < len(test.input); cut++ {
				stream := test.redactor.stream()
				got := stream.Push(test.input[:cut]) + stream.Push(test.input[cut:]) + stream.Flush()
				for _, secret := range test.secrets {
					if strings.Contains(got, secret) {
						t.Fatalf("cut %d leaked %q in %q", cut, secret, got)
					}
				}
				if got != want {
					t.Fatalf("cut %d output = %q, want batch redaction %q", cut, got, want)
				}
			}
		})
	}
}

func TestRedactionStreamMasksStructuralSecretsOneByteAtATime(t *testing.T) {
	input := "safe Authorization: Bearer splitcredential123 tail " +
		"API_KEY='another-secret-value' " +
		"-----BEGIN PRIVATE KEY-----\nkey-body\n-----END PRIVATE KEY----- done"
	stream := (Redactor{}).stream()
	var output strings.Builder
	for index := range len(input) {
		output.WriteString(stream.Push(input[index : index+1]))
		if len(stream.pending) > maxRedactionStreamPendingBytes {
			t.Fatalf("pending bytes = %d, limit %d", len(stream.pending), maxRedactionStreamPendingBytes)
		}
	}
	output.WriteString(stream.Flush())
	got := output.String()
	for _, secret := range []string{"splitcredential123", "another-secret-value", "key-body"} {
		if strings.Contains(got, secret) {
			t.Fatalf("one-byte stream leaked %q in %q", secret, got)
		}
	}
}

func TestRedactionStreamStateIsBoundedForBenignAndSuspiciousInput(t *testing.T) {
	benign := strings.Repeat("ordinary output line\n", 100_000)
	stream := (Redactor{}).stream()
	got := stream.Push(benign)
	if len(stream.pending) > maxRedactionStreamPendingBytes {
		t.Fatalf("benign pending bytes = %d, limit %d", len(stream.pending), maxRedactionStreamPendingBytes)
	}
	got += stream.Flush()
	if got != benign {
		t.Fatalf("benign stream changed: got %d bytes, want %d", len(got), len(benign))
	}

	stream = (Redactor{}).stream()
	got = stream.Push("API_KEY" + strings.Repeat(" ", maxRedactionStreamPendingBytes*4))
	if len(stream.pending) > maxRedactionStreamPendingBytes {
		t.Fatalf("assignment pending bytes = %d, limit %d", len(stream.pending), maxRedactionStreamPendingBytes)
	}
	got += stream.Push("= structural-secret-value tail")
	got += stream.Flush()
	if strings.Contains(got, "structural-secret-value") || !strings.Contains(got, "***") {
		t.Fatalf("long assignment prefix was not fail-safe redacted: %q", got)
	}

	stream = (Redactor{}).stream()
	got = stream.Push("-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("A", maxRedactionStreamPendingBytes*8))
	if len(stream.pending) > maxRedactionStreamPendingBytes {
		t.Fatalf("PEM pending bytes = %d, limit %d", len(stream.pending), maxRedactionStreamPendingBytes)
	}
	got += stream.Push("\n-----END PRIVATE KEY----- visible-tail")
	got += stream.Flush()
	if strings.Contains(got, strings.Repeat("A", 32)) || !strings.Contains(got, "***") || !strings.Contains(got, "visible-tail") {
		t.Fatalf("bounded PEM stream output = %q", got)
	}
}
