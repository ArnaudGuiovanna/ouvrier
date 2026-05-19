package ovr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ouvrier/internal/provider"
	"ouvrier/internal/state"
)

type httpScriptedProvider struct {
	name      string
	requests  []provider.Request
	responses []provider.Response
	response  provider.Response
	err       error
}

func (p *httpScriptedProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "scripted"
}

func (p *httpScriptedProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return provider.Response{}, p.err
	}
	if len(p.responses) > 0 {
		index := len(p.requests) - 1
		if index >= len(p.responses) {
			index = len(p.responses) - 1
		}
		return p.responses[index], nil
	}
	return p.response, nil
}

func TestNewHTTPHandlerExecutesPipeThroughHarnessRuntime(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "ok" || body.Output != `{"status":"classified"}` {
		t.Fatalf("body = %+v, want ok classified JSON", body)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	if scripted.requests[0].Messages[0].Text() != `{"title":"broken"}` {
		t.Fatalf("provider input = %q", scripted.requests[0].Messages[0].Text())
	}
}

func TestNewHTTPHandlerPersistsHarnessStateWhenConfigured(t *testing.T) {
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	sessions, err := store.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions returned error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	execution, ok, err := store.Execution(context.Background(), sessions[0].ExecID)
	if err != nil {
		t.Fatalf("Execution returned error: %v", err)
	}
	if !ok {
		t.Fatal("Execution ok = false, want true")
	}
	if execution.Status != state.ExecutionCompleted {
		t.Fatalf("execution status = %q, want completed", execution.Status)
	}
}

func TestNewHTTPHandlerRecordsOutputSchemaViolation(t *testing.T) {
	store := state.NewMemoryStore()
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Output[httpTestReply](),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted, stateStore: store})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("SchemaViolations returned error: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(violations))
	}
	if violations[0].SchemaName != "ovr.httpTestReply" {
		t.Fatalf("schema name = %q, want ovr.httpTestReply", violations[0].SchemaName)
	}
}

func TestNewHTTPHandlerValidatesTerminalReplySchemaWithoutPipeOutput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":1}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket", Model("anthropic/claude-sonnet-4-6")),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"title":"broken"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	var body httpStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if body.Status != "pipeline_execution_failed" {
		t.Fatalf("status body = %q, want pipeline_execution_failed", body.Status)
	}
}

func TestNewHTTPHandlerPassesPipeToolsToHarnessRuntime(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{Text: `{"status":"classified"}`, StopReason: provider.StopEndTurn},
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify ticket",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup_ticket", func(ctx context.Context, args struct {
				ID string `json:"id"`
			}) (string, error) {
				return "ticket", nil
			}, Describe("Lookup ticket."), Param("id", "Ticket ID.")),
		),
		Reply(JSON[httpTestReply]()),
	}, httpRuntime{provider: scripted})
	if err != nil {
		t.Fatalf("newHTTPHandlerWithRuntime returned error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"id":"T-1"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(scripted.requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(scripted.requests))
	}
	tools := scripted.requests[0].Tools
	if len(tools) != 1 {
		t.Fatalf("provider tools = %d, want 1", len(tools))
	}
	if tools[0].Name != "lookup_ticket" || tools[0].Description != "Lookup ticket." {
		t.Fatalf("tool spec = %+v", tools[0])
	}
	if len(tools[0].InputSchema) == 0 {
		t.Fatal("tool input schema is empty")
	}
}
