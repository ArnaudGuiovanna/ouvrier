package operate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestDecodeModelToolArgumentsValidatesExposedSchema(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		raw      json.RawMessage
		want     map[string]any
		wantErrs []string
	}{
		{
			name: "valid review scope",
			tool: "review_worker",
			raw:  json.RawMessage(`{"scope":"whole_worker","subject":"governance"}`),
			want: map[string]any{"scope": "whole_worker", "subject": "governance"},
		},
		{
			name:     "malformed JSON",
			tool:     "build_worker",
			raw:      json.RawMessage(`{"target":`),
			wantErrs: []string{"decode JSON"},
		},
		{
			name:     "trailing JSON value",
			tool:     "build_worker",
			raw:      json.RawMessage(`{} {}`),
			wantErrs: []string{"single JSON value"},
		},
		{
			name:     "missing required argument",
			tool:     "read_worker_file",
			raw:      json.RawMessage(`{}`),
			wantErrs: []string{"required", "path"},
		},
		{
			name:     "wrong argument type",
			tool:     "search_ouvrier_docs",
			raw:      json.RawMessage(`{"query":42}`),
			wantErrs: []string{"type", "string"},
		},
		{
			name:     "hidden build override",
			tool:     "build_worker",
			raw:      json.RawMessage(`{"allow_failed":true}`),
			wantErrs: []string{"unexpected additional properties", "allow_failed"},
		},
		{
			name:     "hidden transfer override",
			tool:     "transfer_worker",
			raw:      json.RawMessage(`{"env":"staging","allow_failed":true}`),
			wantErrs: []string{"unexpected additional properties", "allow_failed"},
		},
		{
			name:     "unknown argument on no-input tool",
			tool:     "list_workers",
			raw:      json.RawMessage(`{"unexpected":true}`),
			wantErrs: []string{"unexpected additional properties", "unexpected"},
		},
		{
			name:     "write requires content",
			tool:     "write_worker_file",
			raw:      json.RawMessage(`{"path":"main.go"}`),
			wantErrs: []string{"required", "content"},
		},
		{
			name: "valid governed write",
			tool: "write_worker_file",
			raw:  json.RawMessage(`{"path":"main.go","content":"package main\n"}`),
			want: map[string]any{"path": "main.go", "content": "package main\n"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeModelToolArguments(tt.tool, tt.raw)
			if len(tt.wantErrs) > 0 {
				if err == nil {
					t.Fatalf("decodeModelToolArguments() error = nil, want %v", tt.wantErrs)
				}
				for _, part := range tt.wantErrs {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("decodeModelToolArguments() error = %q, want substring %q", err, part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeModelToolArguments() error = %v", err)
			}
			for key, want := range tt.want {
				if got[key] != want {
					t.Errorf("decoded[%q] = %#v, want %#v", key, got[key], want)
				}
			}
		})
	}
}

func TestAgentLoopRejectsOperatorOnlyToolEvenWhenModelNamesIt(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	called := false
	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{
		Name:         "operator_secret",
		Governance:   GovSideEffecting,
		OperatorOnly: true,
		Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
			called = true
			return ToolResult{Summary: "must not run"}, nil
		},
	})
	registry.Register(Tool{
		Name:       "visible_probe",
		Governance: GovReadOnly,
		Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
			return ToolResult{Summary: "visible"}, nil
		},
	})
	model := &scriptedModel{steps: []provider.Response{{
		StopReason: provider.StopToolUse,
		ToolCalls: []provider.ToolCall{{
			ID: "call_hidden", Name: "operator_secret", Arguments: json.RawMessage(`{}`),
		}},
	}}}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "run the hidden tool")
	if err == nil || !strings.Contains(err.Error(), "not callable by the model") {
		t.Fatalf("Prompt() error = %v, want model-callability rejection", err)
	}
	if !strings.Contains(turn.Final, "not callable by the model") {
		t.Fatalf("turn.Final = %q, want explicit rejection", turn.Final)
	}
	if called {
		t.Fatal("operator-only tool executed from a model response")
	}
	entries, readErr := ReadTranscript(started.Session.TranscriptPath)
	if readErr != nil {
		t.Fatalf("ReadTranscript() error = %v", readErr)
	}
	for _, entry := range entries {
		if entry.Kind == TranscriptToolCall && entry.ToolName == "operator_secret" {
			t.Fatalf("operator-only call was persisted as executable: %+v", entry)
		}
	}
}

func TestModelToolSchemasRejectUnknownArguments(t *testing.T) {
	registry := NewToolRegistry()
	for _, name := range registry.Names() {
		tool, ok := registry.Tool(name)
		if !ok || tool.OperatorOnly {
			continue
		}
		if _, err := decodeModelToolArguments(name, json.RawMessage(`{"__unexpected":true}`)); err == nil {
			t.Errorf("tool %q accepted an unknown model argument", name)
		}
	}
}

func TestAgentLoopRejectsInvalidArgumentsBeforeToolExecution(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "malformed JSON", raw: json.RawMessage(`{"unexpected":`), want: "decode JSON"},
		{name: "unknown argument", raw: json.RawMessage(`{"unexpected":true}`), want: "unexpected additional properties"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMinimalWorker(t, dir)

			called := false
			registry := &ToolRegistry{tools: map[string]Tool{}}
			registry.Register(Tool{
				Name:       "probe",
				Governance: GovReadOnly,
				Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
					called = true
					return ToolResult{Summary: "called"}, nil
				},
			})
			model := &scriptedModel{steps: []provider.Response{{
				StopReason: provider.StopToolUse,
				ToolCalls:  []provider.ToolCall{{ID: "call_invalid", Name: "probe", Arguments: tt.raw}},
			}}}
			runtime, err := NewAgentRuntime(RuntimeOptions{
				Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry,
			})
			if err != nil {
				t.Fatalf("NewAgentRuntime() error = %v", err)
			}
			started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}

			turn, err := runtime.Prompt(context.Background(), started.Session.ID, "call the probe")
			if err == nil {
				t.Fatal("Prompt() error = nil, want invalid-arguments error")
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(turn.Final, `tool "probe" arguments are invalid`) {
				t.Fatalf("Prompt() error/final = %q / %q, want explicit validation error containing %q", err, turn.Final, tt.want)
			}
			if called {
				t.Fatal("tool executed despite invalid model arguments")
			}

			entries, err := ReadTranscript(started.Session.TranscriptPath)
			if err != nil {
				t.Fatalf("ReadTranscript() error = %v", err)
			}
			var sawError bool
			for _, entry := range entries {
				if entry.Kind == TranscriptToolCall {
					t.Fatalf("invalid tool call was persisted as executable: %+v", entry)
				}
				if entry.Kind == TranscriptError && entry.ToolName == "probe" {
					sawError = true
				}
			}
			if !sawError {
				t.Fatal("transcript does not contain the rejected tool-call error")
			}
		})
	}
}

func TestAgentLoopRedactsRejectedArgumentValues(t *testing.T) {
	const secret = "model-secret-value"
	dir := t.TempDir()
	writeMinimalWorker(t, dir)

	called := false
	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{
		Name:       "search_ouvrier_docs",
		Governance: GovReadOnly,
		Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
			called = true
			return ToolResult{Summary: "called"}, nil
		},
	})
	model := &scriptedModel{steps: []provider.Response{{
		StopReason: provider.StopToolUse,
		ToolCalls: []provider.ToolCall{{
			ID:        "call_secret",
			Name:      "search_ouvrier_docs",
			Arguments: json.RawMessage(`{"query":["` + secret + `"]}`),
		}},
	}}}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry,
		Redactor: NewRedactor(secret),
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "search the docs")
	if err == nil {
		t.Fatal("Prompt() error = nil, want invalid-arguments error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(turn.Final, secret) {
		t.Fatalf("rejected argument leaked through error/final: %q / %q", err, turn.Final)
	}
	if !strings.Contains(err.Error(), "***") || !strings.Contains(turn.Final, "***") {
		t.Fatalf("error/final = %q / %q, want redaction marker", err, turn.Final)
	}
	if called {
		t.Fatal("tool executed despite invalid model arguments")
	}
}
