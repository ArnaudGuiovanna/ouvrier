package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestAppServerStructuredDynamicToolRoundTrip(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{"serverInfo":{"name":"codex","version":"0.146.0"}}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1","status":"inProgress"}}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":"Je "}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":"lis."}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1,"item":{"id":"message-1","type":"agentMessage","text":"Je lis."}}}`,
		`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":120,"cachedInputTokens":10,"outputTokens":20,"reasoningOutputTokens":4,"totalTokens":140},"total":{"inputTokens":120,"cachedInputTokens":10,"cacheWriteInputTokens":5,"outputTokens":20,"reasoningOutputTokens":4,"totalTokens":140}}}}`,
		`{"method":"item/tool/call","id":"server-call-1","params":{"arguments":{"path":"worker.go"},"callId":"call-1","threadId":"thread-1","tool":"read_worker_file","turnId":"turn-1"}}`,
		`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"message-2","delta":"Terminé."}}`,
		`{"method":"item/completed","params":{"threadId":"thread-1","turnId":"turn-1","completedAtMs":2,"item":{"id":"message-2","type":"agentMessage","text":"Terminé."}}}`,
		`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":80,"cachedInputTokens":50,"outputTokens":15,"reasoningOutputTokens":3,"totalTokens":95},"total":{"inputTokens":200,"cachedInputTokens":60,"cacheWriteInputTokens":5,"outputTokens":35,"reasoningOutputTokens":7,"totalTokens":235}}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}}`,
	)
	transport := &fakeAppServerTransport{process: process}
	p := &AppServerProvider{
		Transport: transport,
		Bin:       "codex-test",
		Model:     "default",
		CWD:       "/workspace/worker",
	}
	t.Cleanup(func() { _ = p.Close() })

	schema := json.RawMessage(`{
  "type": "object",
  "properties": {"path": {"type": "string"}},
  "required": ["path"],
  "additionalProperties": false
}`)
	req := provider.Request{
		Model:  "codex/gpt-5.6-sol",
		System: "Construis un worker Ouvrier.",
		Messages: []provider.Message{
			provider.UserText("Lis worker.go puis termine le travail."),
		},
		Tools: []provider.ToolSpec{{
			Name:        "read_worker_file",
			Description: "Read one worker file.",
			InputSchema: schema,
		}},
	}
	var deltas []string
	first, err := p.CompleteStream(context.Background(), req, func(delta provider.Delta) {
		deltas = append(deltas, delta.Text)
	})
	if err != nil {
		t.Fatalf("first completion: %v", err)
	}
	if first.Text != "Je lis." {
		t.Fatalf("first response text = %q, want authoritative completed item", first.Text)
	}
	if first.StopReason != provider.StopToolUse || len(first.ToolCalls) != 1 {
		t.Fatalf("first response = %#v, want one tool call", first)
	}
	if first.Usage.InputTokens != 120 || first.Usage.OutputTokens != 20 {
		t.Fatalf("first usage = %#v", first.Usage)
	}
	if cache := first.Metadata.PromptCache; !cache.Supported || !cache.Applied || cache.ReadInputTokens != 10 || cache.WriteInputTokens != 5 {
		t.Fatalf("first cache metadata = %#v", cache)
	}
	call := first.ToolCalls[0]
	if call.ID != "call-1" || call.Name != "read_worker_file" || !jsonEqual(call.Arguments, []byte(`{"path":"worker.go"}`)) {
		t.Fatalf("tool call = %#v", call)
	}
	if !reflect.DeepEqual(deltas, []string{"Je ", "lis."}) {
		t.Fatalf("deltas = %#v", deltas)
	}

	continued := req
	continued.Messages = append(append([]provider.Message(nil), req.Messages...),
		provider.AssistantToolCalls(first.Text, call),
		provider.ToolResultMessage(provider.ToolResult{
			ToolCallID: call.ID,
			Name:       call.Name,
			Content:    json.RawMessage(`{"path":"worker.go","content":"package main"}`),
		}),
	)
	final, err := p.Complete(context.Background(), continued)
	if err != nil {
		t.Fatalf("continued completion: %v", err)
	}
	if final.Text != "Terminé." || final.StopReason != provider.StopEndTurn || len(final.ToolCalls) != 0 {
		t.Fatalf("final response = %#v", final)
	}
	if final.Usage.InputTokens != 80 || final.Usage.OutputTokens != 15 {
		t.Fatalf("final incremental usage = %#v", final.Usage)
	}
	if cache := final.Metadata.PromptCache; !cache.Supported || !cache.Applied || cache.ReadInputTokens != 50 || cache.WriteInputTokens != 0 {
		t.Fatalf("final incremental cache metadata = %#v", cache)
	}

	sent := process.Sent()
	if len(sent) != 5 {
		t.Fatalf("sent %d messages, want 5: %s", len(sent), joinJSON(sent))
	}
	for i, message := range sent {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(message, &raw); err != nil {
			t.Fatalf("sent[%d] is not JSON: %v", i, err)
		}
		if _, exists := raw["jsonrpc"]; exists {
			t.Fatalf("sent[%d] unexpectedly contains jsonrpc: %s", i, message)
		}
	}
	assertRPCMethod(t, sent[0], "initialize")
	assertRPCMethod(t, sent[1], "initialized")
	assertRPCMethod(t, sent[2], "thread/start")
	assertRPCMethod(t, sent[3], "turn/start")

	var initialize struct {
		Params struct {
			Capabilities struct {
				ExperimentalAPI bool `json:"experimentalApi"`
			} `json:"capabilities"`
		} `json:"params"`
	}
	decodeSent(t, sent[0], &initialize)
	if !initialize.Params.Capabilities.ExperimentalAPI {
		t.Fatal("initialize did not opt into capabilities.experimentalApi")
	}

	var threadStart struct {
		Params struct {
			Model                 string `json:"model"`
			CWD                   string `json:"cwd"`
			ApprovalPolicy        string `json:"approvalPolicy"`
			Sandbox               string `json:"sandbox"`
			Ephemeral             bool   `json:"ephemeral"`
			BaseInstructions      string `json:"baseInstructions"`
			DeveloperInstructions string `json:"developerInstructions"`
			DynamicTools          []struct {
				Type        string          `json:"type"`
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"dynamicTools"`
			Config map[string]any `json:"config"`
		} `json:"params"`
	}
	decodeSent(t, sent[2], &threadStart)
	if threadStart.Params.Model != "gpt-5.6-sol" || threadStart.Params.CWD != "/workspace/worker" {
		t.Fatalf("thread model/cwd = %q/%q", threadStart.Params.Model, threadStart.Params.CWD)
	}
	if threadStart.Params.ApprovalPolicy != "never" || threadStart.Params.Sandbox != "read-only" || !threadStart.Params.Ephemeral {
		t.Fatalf("thread safety settings = %#v", threadStart.Params)
	}
	if threadStart.Params.BaseInstructions != req.System || !strings.Contains(threadStart.Params.DeveloperInstructions, "sole capability governor") {
		t.Fatalf("thread instructions = base %q developer %q", threadStart.Params.BaseInstructions, threadStart.Params.DeveloperInstructions)
	}
	if len(threadStart.Params.DynamicTools) != 1 {
		t.Fatalf("dynamic tools = %#v", threadStart.Params.DynamicTools)
	}
	tool := threadStart.Params.DynamicTools[0]
	if tool.Type != "function" || tool.Name != req.Tools[0].Name || tool.Description != req.Tools[0].Description || !jsonEqual(tool.InputSchema, schema) {
		t.Fatalf("dynamic tool was not preserved: %#v", tool)
	}
	assertRestrictedConfig(t, threadStart.Params.Config)

	var turnStart struct {
		Params struct {
			ThreadID       string `json:"threadId"`
			ApprovalPolicy string `json:"approvalPolicy"`
			SandboxPolicy  struct {
				Type          string `json:"type"`
				NetworkAccess bool   `json:"networkAccess"`
			} `json:"sandboxPolicy"`
			Input []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	decodeSent(t, sent[3], &turnStart)
	if turnStart.Params.ThreadID != "thread-1" || turnStart.Params.ApprovalPolicy != "never" || turnStart.Params.SandboxPolicy.Type != "readOnly" || turnStart.Params.SandboxPolicy.NetworkAccess {
		t.Fatalf("turn safety settings = %#v", turnStart.Params)
	}
	if len(turnStart.Params.Input) != 1 || !strings.Contains(turnStart.Params.Input[0].Text, "Lis worker.go") {
		t.Fatalf("turn input = %#v", turnStart.Params.Input)
	}

	var toolResponse struct {
		ID     string `json:"id"`
		Result struct {
			Success      bool `json:"success"`
			ContentItems []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"contentItems"`
		} `json:"result"`
	}
	decodeSent(t, sent[4], &toolResponse)
	if toolResponse.ID != "server-call-1" || !toolResponse.Result.Success || len(toolResponse.Result.ContentItems) != 1 {
		t.Fatalf("tool response = %#v", toolResponse)
	}
	if item := toolResponse.Result.ContentItems[0]; item.Type != "inputText" || item.Text != `{"path":"worker.go","content":"package main"}` {
		t.Fatalf("tool response content = %#v", item)
	}
	if transport.name != "codex-test" || !reflect.DeepEqual(transport.args, []string{"app-server", "--listen", "stdio://"}) {
		t.Fatalf("transport command = %q %#v", transport.name, transport.args)
	}
}

func TestAppServerRequiresMatchingToolResultBeforeContinuing(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/tool/call","id":17,"params":{"arguments":{},"callId":"call-1","threadId":"thread-1","tool":"lookup","turnId":"turn-1"}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed"}}}`,
	)
	p := testAppServerProvider(process)
	t.Cleanup(func() { _ = p.Close() })
	req := appServerTestRequest()
	first, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("first completion: %v", err)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", first.ToolCalls)
	}

	if _, err := p.Complete(context.Background(), req); err == nil || !strings.Contains(err.Error(), `result for dynamic tool call "call-1" is required`) {
		t.Fatalf("missing-result error = %v", err)
	}
	if got := len(process.Sent()); got != 4 {
		t.Fatalf("missing result sent a protocol message; got %d", got)
	}

	continued := req
	continued.Messages = append(continued.Messages, provider.ToolResultText(first.ToolCalls[0], "ok", false))
	final, err := p.Complete(context.Background(), continued)
	if err != nil {
		t.Fatalf("retry with result: %v", err)
	}
	if final.StopReason != provider.StopEndTurn {
		t.Fatalf("final stop reason = %q", final.StopReason)
	}
}

func TestAppServerFailsClosedOnPrivilegedServerRequest(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/commandExecution/requestApproval","id":44,"params":{"threadId":"thread-1","turnId":"turn-1","itemId":"command-1","command":"cat /etc/passwd"}}`,
	)
	p := testAppServerProvider(process)

	_, err := p.Complete(context.Background(), appServerTestRequest())
	if err == nil || !strings.Contains(err.Error(), `refused non-dynamic Codex server request "item/commandExecution/requestApproval"`) {
		t.Fatalf("completion error = %v", err)
	}
	if !process.Closed() {
		t.Fatal("privileged request did not terminate app-server process")
	}
	sent := process.Sent()
	if len(sent) != 5 {
		t.Fatalf("sent %d messages, want refusal response: %s", len(sent), joinJSON(sent))
	}
	var refusal struct {
		ID     int `json:"id"`
		Result struct {
			Decision string `json:"decision"`
		} `json:"result"`
	}
	decodeSent(t, sent[4], &refusal)
	if refusal.ID != 44 || refusal.Result.Decision != "cancel" {
		t.Fatalf("refusal = %#v", refusal)
	}
}

func TestAppServerUsesProtocolSpecificRefusals(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{method: "item/commandExecution/requestApproval", want: `{"decision":"cancel"}`},
		{method: "item/fileChange/requestApproval", want: `{"decision":"cancel"}`},
		{method: "item/permissions/requestApproval", want: `{"permissions":{}}`},
		{method: "item/tool/requestUserInput", want: `{"answers":{}}`},
		{method: "mcpServer/elicitation/request", want: `{"action":"cancel","content":null}`},
		{method: "applyPatchApproval", want: `{"decision":"abort"}`},
		{method: "execCommandApproval", want: `{"decision":"abort"}`},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			process := newFakeAppServerProcess()
			p := &AppServerProvider{process: process}
			err := p.rejectServerRequest(context.Background(), rpcEnvelope{
				Method: tt.method,
				ID:     json.RawMessage(`91`),
			})
			if err == nil || !strings.Contains(err.Error(), "Ouvrier refused") {
				t.Fatalf("refusal error = %v", err)
			}
			sent := process.Sent()
			if len(sent) != 1 {
				t.Fatalf("sent = %s", joinJSON(sent))
			}
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			decodeSent(t, sent[0], &response)
			if response.ID != 91 || !jsonEqual(response.Result, []byte(tt.want)) {
				t.Fatalf("response = %s, want result %s", sent[0], tt.want)
			}
		})
	}
}

func TestAppServerRejectsUndeclaredDynamicTool(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/tool/call","id":"bad-call","params":{"arguments":{},"callId":"call-1","threadId":"thread-1","tool":"shell","turnId":"turn-1"}}`,
	)
	p := testAppServerProvider(process)

	_, err := p.Complete(context.Background(), appServerTestRequest())
	if err == nil || !strings.Contains(err.Error(), `undeclared dynamic tool "shell"`) {
		t.Fatalf("completion error = %v", err)
	}
	if !process.Closed() {
		t.Fatal("undeclared tool did not terminate app-server process")
	}
	var refusal struct {
		ID    string `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	sent := process.Sent()
	decodeSent(t, sent[len(sent)-1], &refusal)
	if refusal.ID != "bad-call" || refusal.Error.Code != -32602 {
		t.Fatalf("invalid-call refusal = %#v", refusal)
	}
}

func TestAppServerSurfacesContainmentShutdownFailure(t *testing.T) {
	shutdownErr := errors.New("process group survived")
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/tool/call","id":"bad-call","params":{"arguments":{},"callId":"call-1","threadId":"thread-1","tool":"shell","turnId":"turn-1"}}`,
	)
	process.closeErr = shutdownErr
	p := testAppServerProvider(process)

	_, err := p.Complete(context.Background(), appServerTestRequest())
	if err == nil || !errors.Is(err, shutdownErr) || !strings.Contains(err.Error(), `undeclared dynamic tool "shell"`) {
		t.Fatalf("completion error = %v", err)
	}
}

func TestAppServerCancellationTerminatesProcess(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
	)
	p := testAppServerProvider(process)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := p.Complete(ctx, appServerTestRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completion error = %v, want deadline exceeded", err)
	}
	if !process.Closed() {
		t.Fatal("cancellation did not terminate app-server process")
	}
}

func TestAppServerAbortTurnDropsPendingToolStateAndRestartsFresh(t *testing.T) {
	firstProcess := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-stale"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-stale"}}}`,
		`{"method":"item/tool/call","id":17,"params":{"arguments":{},"callId":"call-stale","threadId":"thread-stale","tool":"lookup","turnId":"turn-stale"}}`,
	)
	secondProcess := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-fresh"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-fresh"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-fresh","turn":{"id":"turn-fresh","status":"completed"}}}`,
	)
	transport := &queuedFakeAppServerTransport{processes: []AppServerProcess{firstProcess, secondProcess}}
	p := &AppServerProvider{Transport: transport, Bin: "codex-test"}
	t.Cleanup(func() { _ = p.Close() })

	first, err := p.Complete(context.Background(), appServerTestRequest())
	if err != nil || len(first.ToolCalls) != 1 {
		t.Fatalf("first completion = %#v, %v", first, err)
	}
	if err := p.AbortTurn(context.Background()); err != nil {
		t.Fatalf("AbortTurn() error = %v", err)
	}
	if !firstProcess.Closed() {
		t.Fatal("AbortTurn did not terminate the process holding stale tool state")
	}

	second, err := p.Complete(context.Background(), appServerTestRequest())
	if err != nil {
		t.Fatalf("fresh completion after abort: %v", err)
	}
	if second.StopReason != provider.StopEndTurn || len(second.ToolCalls) != 0 {
		t.Fatalf("fresh response = %#v", second)
	}
	if starts := transport.StartCount(); starts != 2 {
		t.Fatalf("transport starts = %d, want a fresh process", starts)
	}
}

func TestAppServerCloseInterruptsActiveTurnAndIsIdempotent(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"item/tool/call","id":17,"params":{"arguments":{},"callId":"call-1","threadId":"thread-1","tool":"lookup","turnId":"turn-1"}}`,
	)
	p := testAppServerProvider(process)
	if _, err := p.Complete(context.Background(), appServerTestRequest()); err != nil {
		t.Fatalf("completion: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if !process.Closed() {
		t.Fatal("close did not terminate process")
	}
	// Close is an immediate hard process-group boundary. It intentionally does
	// not enqueue a turn/interrupt request that could block behind active I/O.
	if got := len(process.Sent()); got != 4 {
		t.Fatalf("close sent an unexpected protocol message; got %d", got)
	}
	if _, err := p.Complete(context.Background(), appServerTestRequest()); !errors.Is(err, errAppServerClosed) {
		t.Fatalf("completion after close = %v", err)
	}
}

func TestAppServerCloseInterruptsBlockedCompleteImmediately(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
	)
	p := testAppServerProvider(process)
	completion := make(chan error, 1)
	go func() {
		_, err := p.Complete(context.Background(), appServerTestRequest())
		completion <- err
	}()

	deadline := time.Now().Add(time.Second)
	for len(process.Sent()) < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(process.Sent()); got != 4 {
		_ = process.Close()
		t.Fatalf("completion did not reach the active turn; sent %d messages", got)
	}

	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		_ = process.Close()
		<-closed
		t.Fatal("Close blocked behind the active Complete call")
	}
	select {
	case err := <-completion:
		if !errors.Is(err, errAppServerClosed) {
			t.Fatalf("blocked completion error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("blocked Complete did not return after Close")
	}
}

func TestAppServerQueuedCompleteHonorsContext(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
	)
	p := testAppServerProvider(process)
	first := make(chan error, 1)
	go func() {
		_, err := p.Complete(context.Background(), appServerTestRequest())
		first <- err
	}()

	deadline := time.Now().Add(time.Second)
	for len(process.Sent()) < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(process.Sent()); got != 4 {
		_ = p.Close()
		t.Fatalf("first completion did not reach the active turn; sent %d messages", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := p.Complete(ctx, appServerTestRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		_ = p.Close()
		t.Fatalf("queued completion error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		_ = p.Close()
		t.Fatalf("queued completion ignored its context for %s", elapsed)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-first:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("first completion remained blocked after Close")
	}
}

func TestAppServerRejectsRegressingTokenUsage(t *testing.T) {
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":10,"cachedInputTokens":2,"outputTokens":4,"reasoningOutputTokens":0,"totalTokens":14},"total":{"inputTokens":10,"cachedInputTokens":2,"outputTokens":4,"reasoningOutputTokens":0,"totalTokens":14}}}}`,
		`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"last":{"inputTokens":1,"cachedInputTokens":0,"outputTokens":1,"reasoningOutputTokens":0,"totalTokens":2},"total":{"inputTokens":9,"cachedInputTokens":2,"outputTokens":5,"reasoningOutputTokens":0,"totalTokens":14}}}}`,
	)
	p := testAppServerProvider(process)
	response, err := p.Complete(context.Background(), appServerTestRequest())
	if err == nil || !strings.Contains(err.Error(), "token usage totals regressed") {
		t.Fatalf("completion error = %v", err)
	}
	if response.Usage.InputTokens != 10 || response.Usage.OutputTokens != 4 {
		t.Fatalf("usage captured before protocol failure = %#v", response.Usage)
	}
	if !process.Closed() {
		t.Fatal("invalid usage notification did not terminate app-server")
	}
}

func TestAppServerValidatesDynamicToolsBeforeStartingProcess(t *testing.T) {
	tests := []struct {
		name string
		tool provider.ToolSpec
	}{
		{name: "empty name", tool: provider.ToolSpec{InputSchema: json.RawMessage(`{}`)}},
		{name: "invalid schema", tool: provider.ToolSpec{Name: "lookup", InputSchema: json.RawMessage(`{`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &fakeAppServerTransport{process: newFakeAppServerProcess()}
			p := &AppServerProvider{Transport: transport}
			_, err := p.Complete(context.Background(), provider.Request{
				Messages: []provider.Message{provider.UserText("hello")},
				Tools:    []provider.ToolSpec{tt.tool},
			})
			if err == nil {
				t.Fatal("completion unexpectedly succeeded")
			}
			if transport.started {
				t.Fatal("invalid tool started app-server process")
			}
		})
	}
}

func TestAppServerBoundsNotificationsQueuedDuringRequest(t *testing.T) {
	incoming := make([]string, maxAppServerInbox+1)
	for i := range incoming {
		incoming[i] = `{"method":"warning","params":{"message":"noise"}}`
	}
	process := newFakeAppServerProcess(incoming...)
	p := testAppServerProvider(process)
	_, err := p.Complete(context.Background(), appServerTestRequest())
	if err == nil || !strings.Contains(err.Error(), "notification inbox exceeds") {
		t.Fatalf("completion error = %v", err)
	}
	if !process.Closed() {
		t.Fatal("notification flood did not terminate app-server")
	}
}

func TestAppServerFailsClosedWhenCumulativeTurnTextExceedsLimit(t *testing.T) {
	oversized := strings.Repeat("x", maxAppServerTextBytes+1)
	process := newFakeAppServerProcess(
		`{"id":1,"result":{}}`,
		`{"id":2,"result":{"thread":{"id":"thread-1"}}}`,
		`{"id":3,"result":{"turn":{"id":"turn-1"}}}`,
		fmt.Sprintf(`{"method":"item/agentMessage/delta","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"message-1","delta":%q}}`, oversized),
	)
	p := testAppServerProvider(process)
	var streamed int
	_, err := p.CompleteStream(context.Background(), appServerTestRequest(), func(delta provider.Delta) {
		streamed += len(delta.Text)
	})
	if err == nil || !strings.Contains(err.Error(), "response text exceeds") {
		t.Fatalf("completion error = %v", err)
	}
	if streamed != 0 {
		t.Fatalf("oversized delta streamed %d bytes before validation", streamed)
	}
	if !process.Closed() {
		t.Fatal("oversized turn output did not terminate app-server")
	}
}

func TestAppServerCompletedItemReplacesBufferedDeltaWithinSameBudget(t *testing.T) {
	turn := &appServerTurn{deltas: make(map[string]*strings.Builder)}
	text := strings.Repeat("x", maxAppServerTextBytes-1)
	if err := turn.appendAgentDelta("message-1", text); err != nil {
		t.Fatalf("appendAgentDelta() error = %v", err)
	}
	if err := turn.completeAgentItem("message-1", "agentMessage", text); err != nil {
		t.Fatalf("completeAgentItem() double-counted replacement text: %v", err)
	}
	if got := turn.takeAgentText(); got != text {
		t.Fatalf("completed text bytes = %d, want %d", len(got), len(text))
	}
}

func TestBoundedAppServerStderr(t *testing.T) {
	buffer := newBoundedBuffer(8)
	input := []byte("0123456789abcdef")
	if n, err := buffer.Write(input); err != nil || n != len(input) {
		t.Fatalf("write = %d, %v", n, err)
	}
	if got := buffer.String(); got != "01234567\n[stderr truncated]" {
		t.Fatalf("bounded stderr = %q", got)
	}
}

func TestDefaultAppServerTransportExchangesJSONLAndCloses(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "appserver-transport-helper" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			t.Fatal("helper did not receive a JSONL message")
		}
		var request map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatalf("helper received invalid JSON: %v", err)
		}
		fmt.Println(`{"id":7,"result":{"ok":true}}`)
		return
	}

	process, err := (defaultAppServerTransport{}).Start(
		os.Args[0],
		"-test.run=^TestDefaultAppServerTransportExchangesJSONLAndCloses$",
		"--",
		"appserver-transport-helper",
	)
	if runtime.GOOS != "linux" {
		if err == nil {
			_ = process.Close()
			t.Fatal("app-server transport silently started without proven process-tree containment")
		}
		return
	}
	if err != nil {
		t.Fatalf("start helper transport: %v", err)
	}
	if err := process.Send(context.Background(), []byte(`{"method":"ping","id":7,"params":{}}`)); err != nil {
		t.Fatalf("send JSONL: %v", err)
	}
	message, err := process.Receive(context.Background())
	if err != nil {
		t.Fatalf("receive JSONL: %v", err)
	}
	if !jsonEqual(message, []byte(`{"id":7,"result":{"ok":true}}`)) {
		t.Fatalf("received = %s", message)
	}
	if err := process.Close(); err != nil {
		t.Fatalf("close helper transport: %v", err)
	}
}

func TestStdioAppServerCloseIsBoundedWhenWaitNeverReturns(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	t.Cleanup(func() {
		_ = stdinReader.Close()
		_ = stdoutWriter.Close()
	})
	process := &stdioAppServerProcess{
		cmd:      &exec.Cmd{},
		stdin:    stdinWriter,
		stdout:   stdoutReader,
		stderr:   newBoundedBuffer(maxAppServerStderrBytes),
		messages: make(chan []byte),
		writes:   make(chan appServerWrite),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	started := time.Now()
	err := process.Close()
	if err == nil || !strings.Contains(err.Error(), "did not terminate") {
		t.Fatalf("bounded Close error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > appServerCloseTimeout+250*time.Millisecond {
		t.Fatalf("bounded Close took %s", elapsed)
	}
}

func appServerTestRequest() provider.Request {
	return provider.Request{
		System:   "Use governed tools.",
		Messages: []provider.Message{provider.UserText("lookup")},
		Tools: []provider.ToolSpec{{
			Name:        "lookup",
			Description: "Look up data.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`),
		}},
	}
}

func testAppServerProvider(process *fakeAppServerProcess) *AppServerProvider {
	return &AppServerProvider{
		Transport: &fakeAppServerTransport{process: process},
		Bin:       "codex-test",
	}
}

func assertRestrictedConfig(t *testing.T, config map[string]any) {
	t.Helper()
	if config["web_search"] != "disabled" || config["project_doc_max_bytes"] != float64(0) {
		t.Fatalf("restricted config = %#v", config)
	}
	features, ok := config["features"].(map[string]any)
	if !ok {
		t.Fatalf("features config = %#v", config["features"])
	}
	for _, name := range []string{"apps", "multi_agent", "plugins", "shell_tool", "skill_mcp_dependency_install", "unified_exec"} {
		if value, exists := features[name]; !exists || value != false {
			t.Fatalf("feature %q = %#v, want false", name, value)
		}
	}
	if mcp, ok := config["mcp_servers"].(map[string]any); !ok || len(mcp) != 0 {
		t.Fatalf("mcp_servers = %#v, want empty table", config["mcp_servers"])
	}
}

func assertRPCMethod(t *testing.T, message []byte, want string) {
	t.Helper()
	var envelope struct {
		Method string `json:"method"`
	}
	decodeSent(t, message, &envelope)
	if envelope.Method != want {
		t.Fatalf("RPC method = %q, want %q (%s)", envelope.Method, want, message)
	}
}

func decodeSent(t *testing.T, message []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(message, target); err != nil {
		t.Fatalf("decode sent message %s: %v", message, err)
	}
}

func jsonEqual(left, right []byte) bool {
	var a any
	var b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}

func joinJSON(messages [][]byte) string {
	parts := make([]string, len(messages))
	for i := range messages {
		parts[i] = string(messages[i])
	}
	return strings.Join(parts, "\n")
}

type fakeAppServerTransport struct {
	process *fakeAppServerProcess
	started bool
	name    string
	args    []string
}

type queuedFakeAppServerTransport struct {
	mu        sync.Mutex
	processes []AppServerProcess
	starts    int
}

func (*queuedFakeAppServerTransport) LookPath(file string) (string, error) {
	return "/fake/" + file, nil
}

func (t *queuedFakeAppServerTransport) Start(_ string, _ ...string) (AppServerProcess, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.processes) == 0 {
		return nil, errors.New("no queued fake app-server process")
	}
	process := t.processes[0]
	t.processes = t.processes[1:]
	t.starts++
	return process, nil
}

func (t *queuedFakeAppServerTransport) StartCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.starts
}

func (t *fakeAppServerTransport) LookPath(file string) (string, error) {
	return "/fake/" + file, nil
}

func (t *fakeAppServerTransport) Start(name string, args ...string) (AppServerProcess, error) {
	t.started = true
	t.name = name
	t.args = append([]string(nil), args...)
	return t.process, nil
}

type fakeAppServerProcess struct {
	mu       sync.Mutex
	incoming [][]byte
	next     int
	sent     [][]byte
	stderr   string
	closeErr error
	closed   chan struct{}
	close    sync.Once
}

func newFakeAppServerProcess(incoming ...string) *fakeAppServerProcess {
	messages := make([][]byte, len(incoming))
	for i := range incoming {
		messages[i] = []byte(incoming[i])
	}
	return &fakeAppServerProcess{incoming: messages, closed: make(chan struct{})}
}

func (p *fakeAppServerProcess) Send(ctx context.Context, message []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closed:
		return io.ErrClosedPipe
	default:
	}
	p.mu.Lock()
	p.sent = append(p.sent, append([]byte(nil), message...))
	p.mu.Unlock()
	return nil
}

func (p *fakeAppServerProcess) Receive(ctx context.Context) ([]byte, error) {
	p.mu.Lock()
	if p.next < len(p.incoming) {
		message := append([]byte(nil), p.incoming[p.next]...)
		p.next++
		p.mu.Unlock()
		return message, nil
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.closed:
		return nil, io.EOF
	}
}

func (p *fakeAppServerProcess) Stderr() string { return p.stderr }

func (p *fakeAppServerProcess) Close() error {
	p.close.Do(func() { close(p.closed) })
	return p.closeErr
}

func (p *fakeAppServerProcess) Sent() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([][]byte, len(p.sent))
	for i := range p.sent {
		result[i] = append([]byte(nil), p.sent[i]...)
	}
	return result
}

func (p *fakeAppServerProcess) Closed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}
