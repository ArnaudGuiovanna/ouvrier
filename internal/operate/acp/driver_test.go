package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

type helperRunner struct {
	mode    string
	missing bool
}

func (r helperRunner) LookPath(string) (string, error) {
	if r.missing {
		return "", exec.ErrNotFound
	}
	return os.Executable()
}

func (r helperRunner) CommandContext(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestACPHelperProcess$", "--", "acp-helper="+r.mode)
}

type recordingSink struct {
	mu     sync.Mutex
	events []operate.Event
}

func (s *recordingSink) Event(event operate.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *recordingSink) has(kind operate.EventKind) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func (s *recordingSink) messages(kind operate.EventKind) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var joined strings.Builder
	for _, event := range s.events {
		if event.Kind == kind {
			joined.WriteString(event.Message)
		}
	}
	return joined.String()
}

func TestDriverAppliesStructuredPatchPlanWithoutAgentFilesystemAccess(t *testing.T) {
	dir := t.TempDir()
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "patch-plan"}
	sink := &recordingSink{}

	result, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnPatch, CWD: dir, Sandbox: operate.SandboxWorkspaceWrite,
		Prompt: "update the worker",
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if !strings.Contains(result.FinalMessage, `"summary":"worker updated"`) {
		t.Fatalf("FinalMessage = %q", result.FinalMessage)
	}
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || string(data) != "package main\n\nfunc main() {}\n" {
		t.Fatalf("governed patch plan was not applied: %q, %v", data, err)
	}
	for _, kind := range []operate.EventKind{
		operate.EventAgentDelta, operate.EventCommandStarted,
		operate.EventCommandFinished, operate.EventFileChanged, operate.EventFinal,
	} {
		if !sink.has(kind) {
			t.Errorf("missing normalized event %q", kind)
		}
	}
}

func TestDriverRejectsEditPermissionForReadOnlyTurn(t *testing.T) {
	dir := t.TempDir()
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "edit"}

	result, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnReview, CWD: dir, Sandbox: operate.SandboxReadOnly,
		Prompt: "review only",
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.FinalMessage != "worker edit rejected" {
		t.Fatalf("FinalMessage = %q", result.FinalMessage)
	}
	if _, err := os.Stat(filepath.Join(dir, "acp-change.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only turn changed staged files: %v", err)
	}
}

func TestDriverDoesNotAdvertiseOrServeFilesystemMethods(t *testing.T) {
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "client-fs"}

	result, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnReview, CWD: t.TempDir(), Sandbox: operate.SandboxReadOnly,
		Prompt: "inspect",
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.FinalMessage != "filesystem unavailable" {
		t.Fatalf("FinalMessage = %q", result.FinalMessage)
	}
}

func TestDriverSanitizesAgentEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "must-not-cross-acp")
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "environment"}

	result, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnReview, CWD: t.TempDir(), Sandbox: operate.SandboxReadOnly,
		Prompt: "inspect",
	}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.FinalMessage != "environment clean" || strings.Contains(result.RawOutput, "must-not-cross-acp") {
		t.Fatalf("unsafe ACP environment result: final=%q raw=%q", result.FinalMessage, result.RawOutput)
	}
}

func TestDriverRedactsSecretsSplitAcrossACPChunks(t *testing.T) {
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "split-secret"}
	sink := &recordingSink{}

	result, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnReview, CWD: t.TempDir(), Sandbox: operate.SandboxReadOnly,
		Prompt: "inspect", Redactor: operate.NewRedactor("super-secret-value"),
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	for label, output := range map[string]string{
		"final": result.FinalMessage, "raw": result.RawOutput,
		"events": sink.messages(operate.EventAgentDelta),
	} {
		if output != "***" {
			t.Fatalf("%s output = %q, want one redaction marker", label, output)
		}
	}
}

func TestDriverProbeReportsMissingAdapter(t *testing.T) {
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{missing: true}
	_, err := driver.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "claude-agent-acp") || !strings.Contains(err.Error(), "npm install") {
		t.Fatalf("Probe() error = %v", err)
	}
}

func TestDriverMapsExpiredOAuthToSavedSessionError(t *testing.T) {
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "auth-expired"}
	_, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnReview, CWD: t.TempDir(), Sandbox: operate.SandboxReadOnly,
		Prompt: "inspect",
	}, nil)
	if !errors.Is(err, ErrAuthenticationRequired) || !strings.Contains(err.Error(), "saved local agent session") || strings.Contains(err.Error(), " login") {
		t.Fatalf("RunTurn() error = %v", err)
	}
}

func TestProtocolBudgetCountsBytesBeforeRedaction(t *testing.T) {
	secret := strings.Repeat("s", maxProtocolBytes/2+1)
	client := &client{req: operate.TurnRequest{Redactor: operate.NewRedactor(secret)}}
	if err := client.record([]byte(secret)); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := client.record([]byte(secret)); err == nil {
		t.Fatal("raw ACP bytes bypassed the aggregate limit through redaction")
	}
}

func TestDriverCancellationStopsACPProcess(t *testing.T) {
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "block"}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := driver.RunTurn(ctx, operate.TurnRequest{
		Kind: operate.TurnReview, CWD: t.TempDir(), Sandbox: operate.SandboxReadOnly,
		Prompt: "block",
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunTurn() error = %v, want deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancelled ACP process took %s", elapsed)
	}
}

func TestDriverBoundsAdapterExitAfterCompletedTurn(t *testing.T) {
	driver := New("claude", "claude-agent-acp")
	driver.Runner = helperRunner{mode: "linger"}
	sink := &recordingSink{}
	started := time.Now()
	result, err := driver.RunTurn(context.Background(), operate.TurnRequest{
		Kind: operate.TurnReview, CWD: t.TempDir(), Sandbox: operate.SandboxReadOnly,
		Prompt: "finish then linger",
	}, sink)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.FinalMessage != "worker edit rejected" {
		t.Fatalf("FinalMessage = %q", result.FinalMessage)
	}
	if elapsed := time.Since(started); elapsed > 2*processWait+time.Second {
		t.Fatalf("lingering ACP process took %s", elapsed)
	}
	if !sink.has(operate.EventWarning) {
		t.Fatal("forced adapter cleanup did not emit a warning")
	}
}

func TestACPHelperProcess(t *testing.T) {
	mode := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "acp-helper=") {
			mode = strings.TrimPrefix(arg, "acp-helper=")
		}
	}
	if mode == "" {
		return
	}
	if err := runACPHelper(mode); err != nil {
		_, _ = os.Stderr.WriteString(err.Error())
		os.Exit(92)
	}
	os.Exit(0)
}

func runACPHelper(mode string) error {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	cwd := ""
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage        `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return err
		}
		switch request.Method {
		case "initialize":
			caps, _ := request.Params["clientCapabilities"].(map[string]interface{})
			fs, _ := caps["fs"].(map[string]interface{})
			if fs["readTextFile"] != false || fs["writeTextFile"] != false || caps["terminal"] != false {
				return errors.New("unsafe client capabilities")
			}
			if err := helperResult(encoder, request.ID, map[string]interface{}{
				"protocolVersion":   1,
				"agentCapabilities": map[string]interface{}{},
				"agentInfo":         map[string]interface{}{"name": "fake-acp", "version": "1.0"},
			}); err != nil {
				return err
			}
		case "session/new":
			cwd, _ = request.Params["cwd"].(string)
			if !filepath.IsAbs(cwd) {
				return errors.New("ACP cwd is not absolute")
			}
			meta, _ := request.Params["_meta"].(map[string]interface{})
			claudeCode, _ := meta["claudeCode"].(map[string]interface{})
			options, _ := claudeCode["options"].(map[string]interface{})
			if sources, ok := options["settingSources"].([]interface{}); !ok || len(sources) != 0 {
				return errors.New("ACP session inherited settings sources")
			}
			if tools, ok := options["tools"].([]interface{}); !ok || len(tools) != 0 {
				return errors.New("ACP session did not constrain built-in tools")
			}
			disallowed, _ := options["disallowedTools"].([]interface{})
			if !interfaceSliceContains(disallowed, "Bash") || !interfaceSliceContains(disallowed, "Read") || !interfaceSliceContains(disallowed, "WebFetch") {
				return errors.New("ACP session omitted a risky disallowed tool")
			}
			if err := helperResult(encoder, request.ID, map[string]interface{}{
				"sessionId": "session-test",
				"configOptions": []map[string]interface{}{{
					"id": "mode", "type": "select", "currentValue": "bypassPermissions",
					"options": []map[string]interface{}{{"value": "default", "name": "Default"}},
				}},
			}); err != nil {
				return err
			}
		case "session/set_config_option":
			if request.Params["configId"] != "mode" || request.Params["value"] != "default" {
				return errors.New("ACP permission mode was not forced to default")
			}
			if err := helperResult(encoder, request.ID, map[string]interface{}{"configOptions": []interface{}{}}); err != nil {
				return err
			}
		case "session/prompt":
			if mode == "block" {
				select {}
			}
			if err := encoder.Encode(map[string]interface{}{
				"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]interface{}{"sessionId": "session-test", "update": map[string]interface{}{
					"sessionUpdate": "available_commands_update", "content": []interface{}{},
				}},
			}); err != nil {
				return err
			}
			if mode == "patch-plan" {
				if err := helperMessage(encoder, `{"summary":"worker updated","files":[{"path":"main.go","content":"package main\n\nfunc main() {}\n"}]}`); err != nil {
					return err
				}
				return helperResult(encoder, request.ID, map[string]interface{}{"stopReason": "end_turn"})
			}
			if mode == "environment" {
				message := "environment clean"
				if os.Getenv("ANTHROPIC_API_KEY") != "" {
					message = "environment leaked"
				}
				if err := helperMessage(encoder, message); err != nil {
					return err
				}
				return helperResult(encoder, request.ID, map[string]interface{}{"stopReason": "end_turn"})
			}
			if mode == "split-secret" {
				if err := helperMessage(encoder, "super-"); err != nil {
					return err
				}
				if err := helperMessage(encoder, "secret-value"); err != nil {
					return err
				}
				return helperResult(encoder, request.ID, map[string]interface{}{"stopReason": "end_turn"})
			}
			if mode == "auth-expired" {
				return encoder.Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": request.ID,
					"error": map[string]interface{}{"code": -32603, "message": "Failed to authenticate. API Error: 401 OAuth access token has expired."},
				})
			}
			if mode == "client-fs" {
				if err := encoder.Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": 88, "method": "fs/read_text_file",
					"params": map[string]interface{}{"sessionId": "session-test", "path": "/etc/passwd"},
				}); err != nil {
					return err
				}
				if !scanner.Scan() {
					return scanner.Err()
				}
				var response map[string]interface{}
				if json.Unmarshal(scanner.Bytes(), &response) != nil || response["error"] == nil {
					return errors.New("filesystem request was not rejected")
				}
				if err := helperMessage(encoder, "filesystem unavailable"); err != nil {
					return err
				}
				return helperResult(encoder, request.ID, map[string]interface{}{"stopReason": "end_turn"})
			}
			if err := helperMessage(encoder, "worker "); err != nil {
				return err
			}
			if err := encoder.Encode(map[string]interface{}{
				"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]interface{}{"sessionId": "session-test", "update": map[string]interface{}{
					"sessionUpdate": "tool_call", "toolCallId": "edit-1", "title": "Edit worker", "kind": "edit", "status": "pending",
				}},
			}); err != nil {
				return err
			}
			if err := encoder.Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 77, "method": "session/request_permission",
				"params": map[string]interface{}{
					"sessionId": "session-test", "toolCall": map[string]interface{}{
						"toolCallId": "edit-1", "kind": "edit",
						"rawInput": map[string]interface{}{"file_path": filepath.Join(cwd, "acp-change.txt"), "content": "changed\n"},
					},
					"options": []map[string]interface{}{
						{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
						{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
					},
				},
			}); err != nil {
				return err
			}
			if !scanner.Scan() {
				return scanner.Err()
			}
			var permission struct {
				Result struct {
					Outcome struct {
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				} `json:"result"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &permission); err != nil {
				return err
			}
			final := "edit rejected"
			status := "failed"
			if permission.Result.Outcome.OptionID == "allow" {
				if err := os.WriteFile(filepath.Join(cwd, "acp-change.txt"), []byte("changed\n"), 0o600); err != nil {
					return err
				}
				final = "worker updated"
				status = "completed"
			}
			if err := encoder.Encode(map[string]interface{}{
				"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]interface{}{"sessionId": "session-test", "update": map[string]interface{}{
					"sessionUpdate": "tool_call_update", "toolCallId": "edit-1", "status": status,
					"locations": []map[string]interface{}{{"path": filepath.Join(cwd, "acp-change.txt")}},
				}},
			}); err != nil {
				return err
			}
			if err := helperMessage(encoder, strings.TrimPrefix(final, "worker ")); err != nil {
				return err
			}
			if err := helperResult(encoder, request.ID, map[string]interface{}{"stopReason": "end_turn"}); err != nil {
				return err
			}
			if mode == "linger" {
				select {}
			}
			return nil
		}
	}
	return scanner.Err()
}

func helperMessage(encoder *json.Encoder, text string) error {
	return encoder.Encode(map[string]interface{}{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]interface{}{"sessionId": "session-test", "update": map[string]interface{}{
			"sessionUpdate": "agent_message_chunk", "content": map[string]interface{}{"type": "text", "text": text},
		}},
	})
}

func helperResult(encoder *json.Encoder, id json.RawMessage, result interface{}) error {
	return encoder.Encode(map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result})
}

func interfaceSliceContains(values []interface{}, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
