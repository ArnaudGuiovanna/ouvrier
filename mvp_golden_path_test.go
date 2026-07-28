package ovr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
)

type mvpGoldenReply struct {
	Status   string `json:"status"`
	TicketID string `json:"ticket_id"`
}

type recordingPermissionPolicy struct {
	mu    sync.Mutex
	order *[]string
}

func (p *recordingPermissionPolicy) Authorize(_ context.Context, action PermissionAction) (PermissionDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	*p.order = append(*p.order, "permission:"+action.ToolName+":"+action.ToolCallID)
	return PermissionDecision{Allowed: true, Reason: "golden-path read-only tool"}, nil
}

func TestMVPGoldenPath(t *testing.T) {
	const (
		toolCallID = "toolu_mvp_1"
		ticketID   = "T-53"
	)
	var (
		providerCalls int
		order         []string
	)
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/messages" {
			t.Errorf("provider: path = %q, want /v1/messages", req.URL.Path)
			http.Error(w, "provider: unexpected path", http.StatusNotFound)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("provider: decode request: %v", err)
			http.Error(w, "provider: invalid request", http.StatusBadRequest)
			return
		}
		providerCalls++
		w.Header().Set("Content-Type", "application/json")
		switch providerCalls {
		case 1:
			if tools, ok := body["tools"].([]any); !ok || len(tools) != 1 {
				t.Errorf("provider: tools = %#v, want one governed tool", body["tools"])
			}
			_, _ = fmt.Fprintf(w, `{
				"content":[{
					"type":"tool_use",
					"id":%q,
					"name":"lookup_ticket",
					"input":{"id":%q}
				}],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":3,"output_tokens":5}
			}`, toolCallID, ticketID)
		case 2:
			messages, _ := body["messages"].([]any)
			encoded, _ := json.Marshal(messages)
			if !strings.Contains(string(encoded), "tool_result") || !strings.Contains(string(encoded), ticketID) {
				t.Errorf("provider: second request does not contain real tool result: %s", encoded)
			}
			text := fmt.Sprintf(`{"status":"classified","ticket_id":%q}`, ticketID)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content":     []map[string]any{{"type": "text", "text": text}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 7, "output_tokens": 9},
			})
		default:
			t.Errorf("provider: calls = %d, want exactly 2", providerCalls)
			http.Error(w, "provider: unexpected extra call", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(providerServer.Close)

	anthropic, err := provider.NewAnthropic(provider.AnthropicConfig{
		APIKey:  "hermetic-test-key",
		BaseURL: providerServer.URL,
	})
	if err != nil {
		t.Fatalf("provider: NewAnthropic: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "mvp-state.db")
	store, err := state.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("durable events: open SQLite: %v", err)
	}
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("durable events: create event stream: %v", err)
	}
	runner := NewRunner(WithPermissionPolicy(&recordingPermissionPolicy{order: &order}))
	rt := httpRuntime{provider: anthropic, stateStore: store, eventStream: stream}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("governed tool: configure runner: %v", err)
	}
	toolCalls := 0
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("classify a ticket through the full MVP path",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup_ticket", func(_ context.Context, args struct {
				ID string `json:"id"`
			}) (string, error) {
				toolCalls++
				order = append(order, "tool:"+args.ID)
				return `{"id":"` + args.ID + `","priority":"high"}`, nil
			}, ReadOnly(), Describe("Look up one ticket."), Param("id", "Ticket ID.")),
			Output[mvpGoldenReply](),
		),
		Reply(JSON[mvpGoldenReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("http trigger: build handler: %v", err)
	}

	server := httptest.NewServer(handler)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/tickets", strings.NewReader(`{"id":"T-53"}`))
	if err != nil {
		t.Fatalf("http trigger: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("http trigger: POST /tickets: %v", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("http trigger: read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		executions, _ := store.Executions(context.Background())
		var persisted []events.Event
		if len(executions) > 0 {
			persisted, _ = store.Events(context.Background(), executions[0].ExecID)
		}
		t.Fatalf("http trigger: status = %d body=%s provider_calls=%d order=%v events=%+v, want 200",
			resp.StatusCode, responseBody, providerCalls, order, persisted)
	}
	var response httpStatusResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		t.Fatalf("typed output: decode HTTP response: %v", err)
	}
	var output mvpGoldenReply
	if err := json.Unmarshal([]byte(response.Output), &output); err != nil {
		t.Fatalf("typed output: decode worker output: %v", err)
	}
	if output != (mvpGoldenReply{Status: "classified", TicketID: ticketID}) {
		t.Fatalf("typed output: output = %+v", output)
	}
	if providerCalls != 2 {
		t.Fatalf("provider: calls = %d, want 2", providerCalls)
	}
	if toolCalls != 1 {
		t.Fatalf("governed tool: calls = %d, want 1", toolCalls)
	}
	if len(order) != 2 || !strings.HasPrefix(order[0], "permission:lookup_ticket:"+toolCallID) || order[1] != "tool:"+ticketID {
		t.Fatalf("governed tool: order = %v, want permission before one tool effect", order)
	}

	executions, err := store.Executions(context.Background())
	if err != nil || len(executions) != 1 {
		t.Fatalf("durable events: executions = %+v, err=%v", executions, err)
	}
	execID := executions[0].ExecID
	beforeClose, err := store.Events(context.Background(), execID)
	if err != nil {
		t.Fatalf("durable events: read before close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("durable events: close SQLite: %v", err)
	}
	reopened, err := state.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("durable events: reopen SQLite: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	afterReopen, err := reopened.Events(context.Background(), execID)
	if err != nil {
		t.Fatalf("durable events: read after reopen: %v", err)
	}
	if len(afterReopen) != len(beforeClose) {
		t.Fatalf("durable events: after reopen = %d, before close = %d", len(afterReopen), len(beforeClose))
	}
	required := []events.EventKind{
		events.EventPermissionDecision,
		events.EventToolCallStarted,
		events.EventToolCallCompleted,
		events.EventSchemaValidationPassed,
		events.EventPipelineCompleted,
	}
	var kinds []events.EventKind
	for i, event := range afterReopen {
		kinds = append(kinds, event.Kind)
		if event.ExecID != execID || event.SessionID == "" {
			t.Fatalf("durable events: event[%d] identifiers = %+v", i, event)
		}
		if i > 0 && event.ID <= afterReopen[i-1].ID {
			t.Fatalf("durable events: IDs are not increasing at %d: %d <= %d", i, event.ID, afterReopen[i-1].ID)
		}
	}
	for _, kind := range required {
		if !slices.Contains(kinds, kind) {
			t.Fatalf("durable events: missing %q in %v", kind, kinds)
		}
	}
}

type denyAllPermissionPolicy struct{}

func (denyAllPermissionPolicy) Authorize(_ context.Context, action PermissionAction) (PermissionDecision, error) {
	return PermissionDecision{Allowed: false, Reason: "injected denial for " + action.ToolName}, nil
}

func TestMVPGoldenPathDeniedToolFailsWithoutEffect(t *testing.T) {
	call := provider.ToolCall{
		ID:        "toolu_denied",
		Name:      "lookup_ticket",
		Arguments: json.RawMessage(`{"id":"T-denied"}`),
	}
	scripted := &httpScriptedProvider{
		response: provider.Response{
			StopReason: provider.StopToolUse,
			ToolCalls:  []provider.ToolCall{call},
		},
	}
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("durable events: create event stream: %v", err)
	}
	runner := NewRunner(WithPermissionPolicy(denyAllPermissionPolicy{}))
	rt := httpRuntime{provider: scripted, stateStore: store, eventStream: stream}
	if err := runner.configureHTTPRuntime(&rt); err != nil {
		t.Fatalf("governed tool: configure denial: %v", err)
	}
	toolCalls := 0
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("deny the governed tool",
			Model("anthropic/claude-sonnet-4-6"),
			Tool("lookup_ticket", func(context.Context, struct {
				ID string `json:"id"`
			}) (string, error) {
				toolCalls++
				return "must not run", nil
			}, ReadOnly()),
			Output[mvpGoldenReply](),
		),
		Reply(JSON[mvpGoldenReply]()),
	}, rt)
	if err != nil {
		t.Fatalf("http trigger: build denied handler: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"id":"T-denied"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("governed tool: denied status = %d body=%s, want 502", rec.Code, rec.Body.String())
	}
	if toolCalls != 0 {
		t.Fatalf("governed tool: denied tool calls = %d, want 0", toolCalls)
	}
	var denied, failed bool
	for _, event := range stream.List() {
		switch event.Kind {
		case events.EventPermissionDecision:
			if event.Payload["allowed"] == false {
				denied = true
			}
		case events.EventPipelineFailed:
			failed = true
		}
	}
	if !denied || !failed {
		t.Fatalf("governed tool: denied=%v pipeline_failed=%v events=%+v", denied, failed, stream.List())
	}
}

func TestMVPGoldenPathRejectsInvalidTypedOutput(t *testing.T) {
	scripted := &httpScriptedProvider{
		response: provider.Response{
			Text:       `{"status":42,"ticket_id":"T-invalid"}`,
			StopReason: provider.StopEndTurn,
		},
	}
	store := state.NewMemoryStore()
	stream, err := events.NewEventStream()
	if err != nil {
		t.Fatalf("durable events: create event stream: %v", err)
	}
	handler, err := newHTTPHandlerWithRuntime([]Node{
		From("POST /tickets"),
		Pipe("reject invalid typed output",
			Model("anthropic/claude-sonnet-4-6"),
			Output[mvpGoldenReply](),
		),
		Reply(JSON[mvpGoldenReply]()),
	}, httpRuntime{provider: scripted, stateStore: store, eventStream: stream, schemaRepairAttempts: 0})
	if err != nil {
		t.Fatalf("typed output: build invalid-output handler: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/tickets", strings.NewReader(`{"id":"T-invalid"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("typed output: invalid status = %d body=%s, want 502", rec.Code, rec.Body.String())
	}
	violations, err := store.SchemaViolations(context.Background(), "")
	if err != nil {
		t.Fatalf("typed output: read schema violations: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("typed output: invalid response persisted no schema violation")
	}
	if _, ok := findRuntimeHTTPEvent(stream.List(), events.EventSchemaValidationFailed); !ok {
		t.Fatalf("typed output: missing schema validation failure in %+v", stream.List())
	}
}

func TestMVPGoldenPathBuildsCoveredWorker(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("build worker: Go toolchain unavailable: %v", err)
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("build worker: resolve repository root: %v", err)
	}
	out := filepath.Join(t.TempDir(), "ticket-triage")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = filepath.Join(root, "examples", "ticket-triage")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build worker: go build: %v\n%s", err, output)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("build worker: stat binary: %v", err)
	}
	if info.Size() == 0 || info.Mode()&0o111 == 0 {
		t.Fatalf("build worker: binary size=%d mode=%v, want non-empty executable", info.Size(), info.Mode())
	}
}

func TestMVPGoldenPathStageDiagnostics(t *testing.T) {
	stages := []string{
		"http trigger",
		"provider",
		"governed tool",
		"typed output",
		"durable events",
		"build worker",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			err := mvpStageError(stage, fmt.Errorf("injected regression"))
			if !strings.Contains(err.Error(), stage) || !strings.Contains(err.Error(), "injected regression") {
				t.Fatalf("stage diagnostic = %q, want stage and cause", err)
			}
		})
	}
}

func mvpStageError(stage string, err error) error {
	return fmt.Errorf("%s: %w", stage, err)
}
