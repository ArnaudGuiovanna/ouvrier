package adkspike

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/model"
)

type recordingExecutor struct {
	mu     sync.Mutex
	calls  []ToolCall
	passAt int
	shared map[string]any
}

func (e *recordingExecutor) Execute(_ context.Context, call ToolCall) (ToolResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call)
	verified := e.passAt > 0 && len(e.calls) >= e.passAt
	data := map[string]any{
		"call_id": call.ID,
		"nested":  map[string]any{"status": "original"},
	}
	e.shared = data
	return ToolResult{
		Summary:  fmt.Sprintf("evidence attempt %d", len(e.calls)),
		Verified: verified,
		Data:     data,
	}, nil
}

func (e *recordingExecutor) CallCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

type scriptedLLM struct {
	mu       sync.Mutex
	turns    [][]*model.LLMResponse
	requests int
	err      error
}

func (m *scriptedLLM) Name() string { return "scripted-ouvrier-model" }

func (m *scriptedLLM) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		m.mu.Lock()
		m.requests++
		if m.err != nil {
			err := m.err
			m.mu.Unlock()
			yield(nil, err)
			return
		}
		if len(m.turns) == 0 {
			m.mu.Unlock()
			yield(nil, errors.New("scripted model exhausted"))
			return
		}
		responses := m.turns[0]
		m.turns = m.turns[1:]
		m.mu.Unlock()
		for _, response := range responses {
			if !yield(response, nil) {
				return
			}
		}
	}
}

func (m *scriptedLLM) RequestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

type blockingLLM struct {
	started chan struct{}
	once    sync.Once
}

func (m *blockingLLM) Name() string { return "blocking-ouvrier-model" }

func (m *blockingLLM) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		yield(nil, ctx.Err())
	}
}

func functionCall(id, name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}},
	}}
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}
}

func mustRepairKernel(t *testing.T, cfg RepairConfig, sessionID string) *Kernel {
	t.Helper()
	kernel, err := NewRepairKernel(cfg)
	if err != nil {
		t.Fatalf("NewRepairKernel() error = %v", err)
	}
	if err := kernel.CreateSession(t.Context(), sessionID); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	return kernel
}

func collect(seq iter.Seq2[Event, error]) ([]Event, error) {
	var events []Event
	for event, err := range seq {
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
	return events, nil
}

func assertOrderedKinds(t *testing.T, events []Event, want ...EventKind) {
	t.Helper()
	next := 0
	for _, event := range events {
		if next < len(want) && event.Kind == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Fatalf("event kinds = %v, missing ordered suffix %v", eventKinds(events), want[next:])
	}
}

func assertOutcome(t *testing.T, events []Event, want Outcome) {
	t.Helper()
	outcomes := eventsOfKind(events, EventOutcome)
	if len(outcomes) != 1 {
		t.Fatalf("outcome events = %d, want 1; kinds=%v", len(outcomes), eventKinds(events))
	}
	if outcomes[0].Outcome != want {
		t.Fatalf("outcome = %q, want %q", outcomes[0].Outcome, want)
	}
}

func eventKinds(events []Event) []EventKind {
	out := make([]EventKind, 0, len(events))
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

func findEvent(t *testing.T, events []Event, kind EventKind) Event {
	t.Helper()
	for _, event := range events {
		if event.Kind == kind {
			return event
		}
	}
	t.Fatalf("event %q not found in %v", kind, eventKinds(events))
	return Event{}
}

func eventsOfKind(events []Event, kind EventKind) []Event {
	var out []Event
	for _, event := range events {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func hasKind(events []Event, kind EventKind) bool {
	return len(eventsOfKind(events, kind)) > 0
}

func contains(got, want string) bool {
	return strings.Contains(got, want)
}
