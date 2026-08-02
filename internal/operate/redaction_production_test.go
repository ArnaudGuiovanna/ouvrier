package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestAgentRuntimeProductionRedactorMergesEnvironmentDotenvAndConfiguredValues(t *testing.T) {
	const (
		envSecret        = "env-secret-value"
		dotenvSecret     = "dotenv-secret-value"
		configuredSecret = "configured-secret-value"
	)
	t.Setenv("OPENAI_API_KEY", envSecret)
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("WORKER_WEBHOOK_SECRET="+dotenvSecret+"\nVISIBLE_SETTING=visible-value\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Redactor: NewRedactor(configuredSecret),
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	text := strings.Join([]string{envSecret, dotenvSecret, configuredSecret, "visible-value"}, " ")
	redacted := runtime.Redact(text)
	for _, secret := range []string{envSecret, dotenvSecret, configuredSecret} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("runtime redactor leaked %q in %q", secret, redacted)
		}
		if strings.Contains(runtime.Harness.Redactor.Redact(text), secret) {
			t.Fatalf("harness redactor leaked %q", secret)
		}
	}
	if !strings.Contains(redacted, "visible-value") {
		t.Fatalf("runtime redactor masked a non-secret setting: %q", redacted)
	}
}

func TestTranscriptAndEventLogRedactNestedMetadataAndEventPath(t *testing.T) {
	const secret = "nested-secret-value"
	redactor := NewRedactor(secret)
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	store := NewTranscriptStore(transcriptPath, redactor)
	entry, err := store.Append(TranscriptEntry{
		Kind: TranscriptStatus,
		Text: "message " + secret,
		Metadata: map[string]any{
			"path": "/workspace/" + secret + "/main.go",
			"nested": map[string]any{
				"items": []any{map[string]any{"token": secret}},
			},
		},
	})
	if err != nil {
		t.Fatalf("TranscriptStore.Append() error = %v", err)
	}
	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal(entry) error = %v", err)
	}
	if strings.Contains(string(encodedEntry), secret) {
		t.Fatalf("returned transcript entry leaked secret: %s", encodedEntry)
	}

	eventsPath := filepath.Join(dir, "events.jsonl")
	log := NewEventLog(eventsPath, redactor)
	if err := log.Event(Event{
		Kind:    EventCommandFinished,
		Message: "message " + secret,
		Command: "printf " + secret,
		Path:    "/workspace/" + secret + "/main.go",
		Metadata: map[string]any{
			"nested": []any{map[string]any{"credential": secret}},
			"typed": struct {
				Token string `json:"token"`
			}{Token: secret},
		},
	}); err != nil {
		t.Fatalf("EventLog.Event() error = %v", err)
	}

	for _, path := range []string{transcriptPath, eventsPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s leaked secret: %s", filepath.Base(path), data)
		}
		if !strings.Contains(string(data), "***") {
			t.Fatalf("%s has no redaction marker: %s", filepath.Base(path), data)
		}
	}
}

func TestEventAppendFailureIsReturnedAndStopsDriverTurn(t *testing.T) {
	const secret = "event-path-secret-value"
	blocker := filepath.Join(t.TempDir(), "not-a-directory-"+secret)
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	log := NewEventLog(filepath.Join(blocker, "events.jsonl"), NewRedactor(secret))
	if err := log.Event(Event{Kind: EventFinal, Message: "done"}); err == nil {
		t.Fatal("EventLog.Event() error = nil, want append failure")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("EventLog.Event() leaked secret in error: %v", err)
	}
	if _, err := (ManualDriver{}).RunTurn(context.Background(), TurnRequest{Kind: TurnReview}, log); err == nil {
		t.Fatal("ManualDriver.RunTurn() error = nil, want sink append failure")
	}
}

func TestRunTurnSurfacesRedactedTranscriptAppendFailure(t *testing.T) {
	const secret = "transcript-path-secret-value"
	dir := filepath.Join(t.TempDir(), "worker-"+secret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	writeMinimalWorker(t, dir)
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Redactor: NewRedactor(secret)})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := os.Remove(started.Session.TranscriptPath); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}
	if err := os.Mkdir(started.Session.TranscriptPath, 0o700); err != nil {
		t.Fatalf("replace transcript with directory: %v", err)
	}

	stream, err := runtime.RunTurn(context.Background(), started.Session.ID, "/policy", "prompt")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	var sawError, sawDone bool
	for event := range stream {
		if event.Err != nil && strings.Contains(event.Err.Error(), secret) {
			t.Fatalf("StreamEvent.Err leaked secret: %v", event.Err)
		}
		switch event.Kind {
		case StreamError:
			sawError = event.Err != nil
		case StreamDone:
			sawDone = event.Err != nil
		}
	}
	if !sawError || !sawDone {
		t.Fatalf("stream did not surface append failure: error=%v done=%v", sawError, sawDone)
	}
}

func TestRuntimeRedactsEveryStreamAndPersistedToolSurface(t *testing.T) {
	const secret = "stream-secret-value"
	t.Setenv("OUVRIER_ADMIN_TOKEN", secret)
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{
		Name: "probe", Governance: GovReadOnly,
		Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
			return ToolResult{
				Summary: "tool summary " + secret,
				Data: map[string]any{
					"command": "printf " + secret,
					"nested":  map[string]any{"token": secret},
				},
			}, nil
		},
	})
	model := &splitSecretModel{secret: secret}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Tools: registry, Model: model, ModelID: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	stream, err := runtime.RunTurn(context.Background(), started.Session.ID, "inspect "+secret, "prompt")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	var deltas strings.Builder
	for event := range stream {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("Marshal(StreamEvent) error = %v", err)
		}
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("StreamEvent leaked secret: %s", encoded)
		}
		if event.Err != nil && strings.Contains(event.Err.Error(), secret) {
			t.Fatalf("StreamEvent.Err leaked secret: %v", event.Err)
		}
		if event.Kind == StreamAssistantDelta {
			deltas.WriteString(event.Delta)
		}
	}
	if !strings.Contains(deltas.String(), "***") {
		t.Fatalf("streamed deltas = %q, want redaction marker", deltas.String())
	}
	if len(model.requests) < 2 {
		t.Fatalf("provider requests = %d, want tool-result round trip", len(model.requests))
	}
	providerPayload, err := json.Marshal(model.requests[1])
	if err != nil {
		t.Fatalf("Marshal(provider request) error = %v", err)
	}
	if strings.Contains(string(providerPayload), secret) {
		t.Fatalf("tool result sent a secret to the provider: %s", providerPayload)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "finish "+secret)
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	encodedTurn, err := json.Marshal(turn)
	if err != nil {
		t.Fatalf("Marshal(RuntimeTurn) error = %v", err)
	}
	if strings.Contains(string(encodedTurn), secret) {
		t.Fatalf("RuntimeTurn leaked secret: %s", encodedTurn)
	}

	for _, path := range []string{started.Session.TranscriptPath, started.Session.ToolCallsPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s leaked secret: %s", filepath.Base(path), data)
		}
	}
}

type splitSecretModel struct {
	secret   string
	step     int
	requests []provider.Request
}

func (m *splitSecretModel) Complete(_ context.Context, req provider.Request, onDelta func(string)) (provider.Response, error) {
	m.requests = append(m.requests, req)
	cut := len(m.secret) / 2
	if onDelta != nil {
		onDelta("delta " + m.secret[:cut])
		onDelta(m.secret[cut:])
	}
	m.step++
	if m.step == 1 {
		return provider.Response{
			Text:       "assistant " + m.secret,
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{{ID: "probe", Name: "probe", Arguments: json.RawMessage(`{}`)}},
		}, nil
	}
	return provider.Response{Text: "final " + m.secret, StopReason: provider.StopEndTurn}, nil
}

func TestRedactedErrorPreservesErrorsIsOnlyForUnchangedErrors(t *testing.T) {
	base := errors.New("safe sentinel")
	if got := redactError(NewRedactor("unrelated"), base); !errors.Is(got, base) {
		t.Fatalf("redactError() lost unchanged error identity: %v", got)
	}
	secretErr := errors.New("failure includes secret-value")
	got := redactError(NewRedactor("secret-value"), secretErr)
	if errors.Is(got, secretErr) {
		t.Fatalf("redactError() retained a secret-bearing unwrap target: %v", got)
	}
	if strings.Contains(got.Error(), "secret-value") || !strings.Contains(got.Error(), "***") {
		t.Fatalf("redactError() = %q, want masked error", got)
	}
}
