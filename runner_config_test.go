package ovr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestRunnerStateStoreConfiguresHTTPRuntime(t *testing.T) {
	store := newRecordingStateStore()
	runner := NewRunner(WithStateStore(store))
	rt, closeRuntime, err := runner.defaultHTTPRuntimeForRun()
	if err != nil {
		t.Fatalf("defaultHTTPRuntimeForRun returned error: %v", err)
	}
	defer func() {
		if err := closeRuntime(); err != nil {
			t.Fatalf("closeRuntime returned error: %v", err)
		}
	}()
	rt.provider = &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	rt.providers = nil
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s events=%+v", rec.Code, http.StatusOK, rec.Body.String(), rt.eventStream.List())
	}
	if len(store.executions) != 1 {
		t.Fatalf("executions = %+v, want one runner execution", store.executions)
	}
	if len(store.sessions) != 2 {
		t.Fatalf("sessions = %+v, want pipeline and pipe sessions", store.sessions)
	}
	if len(store.events) == 0 {
		t.Fatal("events is empty, want runner events persisted to custom store")
	}
}

func TestRunnerStateStoreSeedsEventStreamFromExistingEvents(t *testing.T) {
	store := newRecordingStateStore()
	store.events = append(store.events, Event{
		ID:     41,
		Kind:   EventSessionStarted,
		ExecID: "exec_existing",
	})
	runner := NewRunner(WithStateStore(store))
	rt, closeRuntime, err := runner.defaultHTTPRuntimeForRun()
	if err != nil {
		t.Fatalf("defaultHTTPRuntimeForRun returned error: %v", err)
	}
	defer func() {
		if err := closeRuntime(); err != nil {
			t.Fatalf("closeRuntime returned error: %v", err)
		}
	}()

	if err := rt.emitRuntimeEvent(context.Background(), planRunResult{}, internalEvent(Event{Kind: EventHookFailed}).Kind, map[string]any{
		"blocked_kind": string(EventPipelineStarted),
		"error":        "hook pipeline_started blocked event: audit denied",
	}); err != nil {
		t.Fatalf("emitRuntimeEvent returned error: %v", err)
	}
	if got := store.events[len(store.events)-1].ID; got != 42 {
		t.Fatalf("new event ID = %d, want 42 after existing event ID 41", got)
	}

	handler := newTestAdminHTTPHandler(t, rt)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/traces?last=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Traces []struct {
			LastEventID uint64 `json:"last_event_id"`
			LastKind    string `json:"last_kind"`
		} `json:"traces"`
	}
	decodeAdminJSON(t, rec, &body)
	if body.Status != "ok" || len(body.Traces) != 1 {
		t.Fatalf("body = %+v, want one recent trace", body)
	}
	if body.Traces[0].LastEventID != 42 || body.Traces[0].LastKind != string(EventHookFailed) {
		t.Fatalf("trace = %+v, want hook_failed at event ID 42", body.Traces[0])
	}
}

func TestRunnerHooksConfigureHTTPRuntime(t *testing.T) {
	hooks := NewHooks()
	seen := false
	if err := hooks.Register(EventPipelineStarted, func(ctx context.Context, event Event) (Event, error) {
		seen = true
		event.Payload["public_hook"] = true
		return event, nil
	}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	runner := NewRunner(WithHooks(hooks))
	rt, closeRuntime, err := runner.defaultHTTPRuntimeForRun()
	if err != nil {
		t.Fatalf("defaultHTTPRuntimeForRun returned error: %v", err)
	}
	defer func() {
		if err := closeRuntime(); err != nil {
			t.Fatalf("closeRuntime returned error: %v", err)
		}
	}()
	rt.provider = &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	rt.providers = nil
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s events=%+v", rec.Code, http.StatusOK, rec.Body.String(), rt.eventStream.List())
	}
	if !seen {
		t.Fatal("public hook was not called")
	}
	started, ok := findRuntimeHTTPEvent(rt.eventStream.List(), internalEvent(Event{Kind: EventPipelineStarted}).Kind)
	if !ok {
		t.Fatalf("events = %+v, want pipeline started event", rt.eventStream.List())
	}
	if started.Payload["public_hook"] != true {
		t.Fatalf("pipeline started payload = %+v, want public hook enrichment", started.Payload)
	}
}

func TestRunnerSchemaRepairAttemptsRejectsNegativeValue(t *testing.T) {
	runner := NewRunner(WithSchemaRepairAttempts(-1))

	err := runner.Run(
		"127.0.0.1:bad-port",
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want invalid runner option")
	}
	if !strings.Contains(err.Error(), "schema repair attempts must be greater than or equal to zero") {
		t.Fatalf("Run error = %v, want schema repair attempts context", err)
	}
}

func TestRunnerSchemaRepairAttemptsConfiguresHTTPRuntime(t *testing.T) {
	runner := NewRunner(WithSchemaRepairAttempts(2))
	rt := defaultHTTPRuntime()

	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}
	if rt.schemaRepairAttempts != 2 {
		t.Fatalf("schemaRepairAttempts = %d, want 2", rt.schemaRepairAttempts)
	}
}

type recordingStateStore struct {
	executions  map[string]Execution
	sessions    map[string]Session
	idempotency map[string]string
	events      []Event
	nextEventID uint64
	violations  []SchemaViolation
	memory      map[string]map[string]MemoryRecord
}

func newRecordingStateStore() *recordingStateStore {
	return &recordingStateStore{
		executions:  map[string]Execution{},
		sessions:    map[string]Session{},
		idempotency: map[string]string{},
		memory:      map[string]map[string]MemoryRecord{},
	}
}

func (s *recordingStateStore) SaveExecution(ctx context.Context, execution Execution) error {
	s.executions[execution.ExecID] = execution
	return nil
}

func (s *recordingStateStore) Execution(ctx context.Context, execID string) (Execution, bool, error) {
	execution, ok := s.executions[execID]
	return execution, ok, nil
}

func (s *recordingStateStore) Executions(ctx context.Context) ([]Execution, error) {
	executions := make([]Execution, 0, len(s.executions))
	for _, execution := range s.executions {
		executions = append(executions, execution)
	}
	return executions, nil
}

func (s *recordingStateStore) SaveSession(ctx context.Context, session Session) error {
	s.sessions[session.SessionID] = session
	return nil
}

func (s *recordingStateStore) Session(ctx context.Context, sessionID string) (Session, bool, error) {
	session, ok := s.sessions[sessionID]
	return session, ok, nil
}

func (s *recordingStateStore) Sessions(ctx context.Context) ([]Session, error) {
	sessions := make([]Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (s *recordingStateStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
	if existing := s.idempotency[key]; existing != "" {
		return existing, false, nil
	}
	s.idempotency[key] = execID
	return "", true, nil
}

func (s *recordingStateStore) AddEvent(ctx context.Context, event Event) (Event, error) {
	if event.ID == 0 {
		s.nextEventID++
		event.ID = s.nextEventID
	}
	s.events = append(s.events, event)
	return event, nil
}

func (s *recordingStateStore) Events(ctx context.Context, execID string) ([]Event, error) {
	return s.EventsSince(ctx, execID, 0)
}

func (s *recordingStateStore) EventsSince(ctx context.Context, execID string, afterID uint64) ([]Event, error) {
	events := make([]Event, 0, len(s.events))
	for _, event := range s.events {
		if event.ID > afterID && (execID == "" || event.ExecID == execID) {
			events = append(events, event)
		}
	}
	return events, nil
}

func (s *recordingStateStore) AddSchemaViolation(ctx context.Context, violation SchemaViolation) (SchemaViolation, error) {
	violation.ID = uint64(len(s.violations) + 1)
	s.violations = append(s.violations, violation)
	return violation, nil
}

func (s *recordingStateStore) SchemaViolations(ctx context.Context, execID string) ([]SchemaViolation, error) {
	violations := make([]SchemaViolation, 0, len(s.violations))
	for _, violation := range s.violations {
		if execID == "" || violation.ExecID == execID {
			violations = append(violations, violation)
		}
	}
	return violations, nil
}

func (s *recordingStateStore) SaveMemory(ctx context.Context, scope, key, value string) error {
	if s.memory[scope] == nil {
		s.memory[scope] = map[string]MemoryRecord{}
	}
	s.memory[scope][key] = MemoryRecord{Scope: scope, Key: key, Value: value}
	return nil
}

func (s *recordingStateStore) Memory(ctx context.Context, scope, key string) (string, bool, error) {
	record, ok := s.memory[scope][key]
	return record.Value, ok, nil
}

func (s *recordingStateStore) ListMemory(ctx context.Context, scope string) ([]MemoryRecord, error) {
	records := make([]MemoryRecord, 0, len(s.memory[scope]))
	for _, record := range s.memory[scope] {
		records = append(records, record)
	}
	return records, nil
}

func TestRunnerPricingRejectsEmptyTable(t *testing.T) {
	runner := NewRunner(WithPricing(PricingTable{}))

	err := runner.Run(
		"127.0.0.1:bad-port",
		From("GET /health"),
		Reply(JSON[httpTestReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want invalid runner option")
	}
	if !strings.Contains(err.Error(), "pricing table is required") {
		t.Fatalf("Run error = %v, want pricing table context", err)
	}
}

func TestRunnerPricingConfiguresHTTPRuntime(t *testing.T) {
	table := PricingTable{
		"anthropic/claude-sonnet-4-6": PerMillion(3, 15, 0.30, 3.75),
	}
	runner := NewRunner(WithPricing(table))
	rt := defaultHTTPRuntime()

	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("configureHTTPRuntime returned error: %v", err)
	}
	if len(rt.pricing) != 1 {
		t.Fatalf("pricing entries = %d, want 1", len(rt.pricing))
	}
	if _, ok := rt.pricing["anthropic/claude-sonnet-4-6"]; !ok {
		t.Fatalf("pricing missing expected model rate: %+v", rt.pricing)
	}
}
