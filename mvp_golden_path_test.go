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
	"reflect"
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
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(mvpStageError("provider", fmt.Errorf("decode request: %w", err)))
			http.Error(w, "provider: invalid request", http.StatusBadRequest)
			return
		}
		providerCalls++
		if err := mvpValidateProviderRequest(req.URL.Path, body, providerCalls, ticketID); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch providerCalls {
		case 1:
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
			text := fmt.Sprintf(`{"status":"classified","ticket_id":%q}`, ticketID)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content":     []map[string]any{{"type": "text", "text": text}},
				"stop_reason": "end_turn",
				"usage":       map[string]any{"input_tokens": 7, "output_tokens": 9},
			})
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
	t.Cleanup(server.Close)
	responseBody, err := mvpTriggerHTTP(server.Client(), server.URL+"/tickets")
	if err != nil {
		executions, _ := store.Executions(context.Background())
		var persisted []events.Event
		if len(executions) > 0 {
			persisted, _ = store.Events(context.Background(), executions[0].ExecID)
		}
		t.Fatalf("%v; provider_calls=%d order=%v events=%+v", err, providerCalls, order, persisted)
	}
	if _, err := mvpValidateTypedOutput(responseBody, mvpGoldenReply{Status: "classified", TicketID: ticketID}); err != nil {
		t.Fatal(err)
	}
	if providerCalls != 2 {
		t.Fatalf("provider: calls = %d, want 2", providerCalls)
	}
	if err := mvpValidateGovernedTool(toolCalls, order, toolCallID, ticketID); err != nil {
		t.Fatal(err)
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
	if err := mvpValidateDurableEvents(beforeClose, afterReopen, execID); err != nil {
		t.Fatal(err)
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
	if err := mvpBuildCoveredWorker(root, out, mvpGoBuild); err != nil {
		t.Fatal(err)
	}
}

func TestMVPGoldenPathStageDiagnostics(t *testing.T) {
	stages := []struct {
		name      string
		wantCause string
		inject    func() error
	}{
		{
			name:      "http trigger",
			wantCause: "injected-invalid-url",
			inject: func() error {
				_, err := mvpTriggerHTTP(http.DefaultClient, "://injected-invalid-url")
				return err
			},
		},
		{
			name:      "provider",
			wantCause: "injected-wrong-path",
			inject: func() error {
				return mvpValidateProviderRequest("/injected-wrong-path", nil, 1, "T-53")
			},
		},
		{
			name:      "governed tool",
			wantCause: "calls = 0",
			inject: func() error {
				return mvpValidateGovernedTool(0, nil, "toolu_mvp_1", "T-53")
			},
		},
		{
			name:      "typed output",
			wantCause: "cannot unmarshal number",
			inject: func() error {
				_, err := mvpValidateTypedOutput(
					[]byte(`{"status":"ok","output":"{\"status\":42,\"ticket_id\":\"T-53\"}"}`),
					mvpGoldenReply{Status: "classified", TicketID: "T-53"},
				)
				return err
			},
		},
		{
			name:      "durable events",
			wantCause: "after reopen = 0",
			inject: func() error {
				before := []events.Event{{ID: 1, ExecID: "exec-53", SessionID: "session-53"}}
				return mvpValidateDurableEvents(before, nil, "exec-53")
			},
		},
		{
			name:      "build worker",
			wantCause: "injected compiler failure",
			inject: func() error {
				return mvpBuildCoveredWorker("", "", func(_, _ string) ([]byte, error) {
					return nil, fmt.Errorf("injected compiler failure")
				})
			},
		},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			err := stage.inject()
			if err == nil {
				t.Fatalf("injected %s regression produced no error", stage.name)
			}
			if !strings.Contains(err.Error(), stage.name) || !strings.Contains(err.Error(), stage.wantCause) {
				t.Fatalf("stage diagnostic = %q, want %q and cause %q", err, stage.name, stage.wantCause)
			}
		})
	}
}

func mvpTriggerHTTP(client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"id":"T-53"}`))
	if err != nil {
		return nil, mvpStageError("http trigger", fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, mvpStageError("http trigger", fmt.Errorf("POST /tickets: %w", err))
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, mvpStageError("http trigger", fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, mvpStageError(
			"http trigger",
			fmt.Errorf("status = %d body=%s, want 200", resp.StatusCode, responseBody),
		)
	}
	return responseBody, nil
}

func mvpValidateProviderRequest(path string, body map[string]any, call int, ticketID string) error {
	if path != "/v1/messages" {
		return mvpStageError("provider", fmt.Errorf("path = %q, want /v1/messages", path))
	}
	switch call {
	case 1:
		if tools, ok := body["tools"].([]any); !ok || len(tools) != 1 {
			return mvpStageError("provider", fmt.Errorf("tools = %#v, want one governed tool", body["tools"]))
		}
	case 2:
		messages, _ := body["messages"].([]any)
		encoded, _ := json.Marshal(messages)
		if !strings.Contains(string(encoded), "tool_result") || !strings.Contains(string(encoded), ticketID) {
			return mvpStageError(
				"provider",
				fmt.Errorf("second request does not contain real tool result: %s", encoded),
			)
		}
	default:
		return mvpStageError("provider", fmt.Errorf("calls = %d, want exactly 2", call))
	}
	return nil
}

func mvpValidateGovernedTool(toolCalls int, order []string, toolCallID, ticketID string) error {
	if toolCalls != 1 {
		return mvpStageError("governed tool", fmt.Errorf("calls = %d, want 1", toolCalls))
	}
	if len(order) != 2 ||
		!strings.HasPrefix(order[0], "permission:lookup_ticket:"+toolCallID) ||
		order[1] != "tool:"+ticketID {
		return mvpStageError(
			"governed tool",
			fmt.Errorf("order = %v, want permission before one tool effect", order),
		)
	}
	return nil
}

func mvpValidateTypedOutput(responseBody []byte, want mvpGoldenReply) (mvpGoldenReply, error) {
	var response httpStatusResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return mvpGoldenReply{}, mvpStageError("typed output", fmt.Errorf("decode HTTP response: %w", err))
	}
	var output mvpGoldenReply
	if err := json.Unmarshal([]byte(response.Output), &output); err != nil {
		return mvpGoldenReply{}, mvpStageError("typed output", fmt.Errorf("decode worker output: %w", err))
	}
	if output != want {
		return mvpGoldenReply{}, mvpStageError("typed output", fmt.Errorf("output = %+v, want %+v", output, want))
	}
	return output, nil
}

func mvpValidateDurableEvents(beforeClose, afterReopen []events.Event, execID string) error {
	if len(afterReopen) != len(beforeClose) {
		return mvpStageError(
			"durable events",
			fmt.Errorf("after reopen = %d, before close = %d", len(afterReopen), len(beforeClose)),
		)
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
		if !reflect.DeepEqual(event, beforeClose[i]) {
			return mvpStageError(
				"durable events",
				fmt.Errorf("event[%d] changed after reopen: before=%+v after=%+v", i, beforeClose[i], event),
			)
		}
		if event.ExecID != execID || event.SessionID == "" {
			return mvpStageError("durable events", fmt.Errorf("event[%d] identifiers = %+v", i, event))
		}
		if i > 0 && event.ID <= afterReopen[i-1].ID {
			return mvpStageError(
				"durable events",
				fmt.Errorf("IDs are not increasing at %d: %d <= %d", i, event.ID, afterReopen[i-1].ID),
			)
		}
	}
	for _, kind := range required {
		if !slices.Contains(kinds, kind) {
			return mvpStageError("durable events", fmt.Errorf("missing %q in %v", kind, kinds))
		}
	}
	return nil
}

type mvpBuildRunner func(workDir, outputPath string) ([]byte, error)

func mvpBuildCoveredWorker(root, out string, build mvpBuildRunner) error {
	output, err := build(filepath.Join(root, "examples", "ticket-triage"), out)
	if err != nil {
		return mvpStageError("build worker", fmt.Errorf("go build: %w\n%s", err, output))
	}
	info, err := os.Stat(out)
	if err != nil {
		return mvpStageError("build worker", fmt.Errorf("stat binary: %w", err))
	}
	if info.Size() == 0 || info.Mode()&0o111 == 0 {
		return mvpStageError(
			"build worker",
			fmt.Errorf("binary size=%d mode=%v, want non-empty executable", info.Size(), info.Mode()),
		)
	}
	return nil
}

func mvpGoBuild(workDir, outputPath string) ([]byte, error) {
	cmd := exec.Command("go", "build", "-o", outputPath, ".")
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	return cmd.CombinedOutput()
}

func mvpStageError(stage string, err error) error {
	return fmt.Errorf("%s: %w", stage, err)
}
