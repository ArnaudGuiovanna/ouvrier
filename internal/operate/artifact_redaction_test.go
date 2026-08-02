package operate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactMapMasksSensitiveKeysWithoutKnownValues(t *testing.T) {
	got := redactMap(Redactor{}, map[string]any{
		"authorization": "Bearer unknown-secret",
		"api_key":       "unknown-secret",
		"nested": map[string]any{
			"client-secret": "unknown-secret",
			"token_count":   42,
		},
	})
	if got["authorization"] != "***" || got["api_key"] != "***" {
		t.Fatalf("top-level structural redaction = %#v", got)
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["client-secret"] != "***" || nested["token_count"] != 42 {
		t.Fatalf("nested structural redaction = %#v", got["nested"])
	}
}

func TestRedactValueMasksSensitiveKeysInTypedStringMap(t *testing.T) {
	got, ok := redactValue(Redactor{}, map[string]string{
		"api_key":     "unknown-typed-secret",
		"token_count": "42",
		"ordinary":    "visible",
	}).(map[string]string)
	if !ok {
		t.Fatalf("redactValue() type = %T", got)
	}
	if got["api_key"] != "***" || got["ordinary"] != "visible" || got["token_count"] != "42" {
		t.Fatalf("typed string map redaction = %#v", got)
	}
}

func TestRedactorMasksHighConfidenceUnknownCredentialShapes(t *testing.T) {
	input := strings.Join([]string{
		`password = "hunter2-secret"`,
		`"client_secret":"never-loaded-value"`,
		`Authorization: Bearer abcdefghijklmnop`,
		`sk-abcdefghijklmnopqrstuvwx`,
		"-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
	}, "\n")
	got := (Redactor{}).Redact(input)
	for _, leaked := range []string{"hunter2-secret", "never-loaded-value", "abcdefghijklmnop", "sk-abcdefghijklmnopqrstuvwx", "private-material"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("Redact() leaked %q in %q", leaked, got)
		}
	}
	if strings.Count(got, "***") < 5 {
		t.Fatalf("Redact() = %q, want all credential shapes masked", got)
	}
}

func TestDurableReportsRedactFreeFormOutput(t *testing.T) {
	dir := t.TempDir()
	secret := "artifact-super-secret"
	redactor := NewRedactor(secret)
	writes := []struct {
		name  string
		write func(string) error
	}{
		{"audit.json", func(path string) error {
			return WriteAuditReport(path, AuditReport{Results: []GateResult{{Name: "test", Output: secret, Error: secret}}}, redactor)
		}},
		{"review.json", func(path string) error {
			return WriteReviewReport(path, ReviewReport{Summary: secret, Raw: secret, Findings: []Finding{{Title: secret, Body: secret}}}, redactor)
		}},
		{"patch.json", func(path string) error {
			return WritePatchReport(path, PatchReport{Goal: secret, Summary: secret, Raw: secret, Diff: CandidateDiff{Diff: secret}}, redactor)
		}},
		{"transfer.json", func(path string) error {
			return WriteTransferReport(path, TransferReport{Error: secret, Request: TransferRequest{EnvFile: secret}}, redactor)
		}},
	}
	for _, tc := range writes {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := tc.write(path); err != nil {
				t.Fatalf("write report: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read report: %v", err)
			}
			if strings.Contains(string(data), secret) || !strings.Contains(string(data), "***") {
				t.Fatalf("persisted report was not redacted: %s", data)
			}
		})
	}
}

func TestStoreRedactsLastErrorBeforePersistence(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	secret := "session-super-secret"
	store.redactor = NewRedactor(secret)
	session, err := store.Create(t.TempDir(), "manual", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	session.LastError = "provider failed with " + secret
	if err := store.Save(session); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(store.SessionDir(session.ID), "session.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "***") {
		t.Fatalf("session.json leaked LastError: %s", data)
	}
}
