package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func startExecutorRuntime(t *testing.T, opts RuntimeOptions) (*AgentRuntime, *Session) {
	t.Helper()
	rt, err := NewAgentRuntime(opts)
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: opts.Dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return rt, started.Session
}

func readToolCallRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read tool-calls: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse tool-call record %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

func transcriptToolEntries(t *testing.T, path, tool string) (calls, results []TranscriptEntry) {
	t.Helper()
	entries, err := ReadTranscript(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	for _, e := range entries {
		if e.ToolName != tool {
			continue
		}
		switch e.Kind {
		case TranscriptToolCall:
			calls = append(calls, e)
		case TranscriptToolResult:
			results = append(results, e)
		}
	}
	return calls, results
}

// A denied approval must block execution and still write exactly one audit
// record plus the tool_call/tool_result transcript pair.
func TestExecutorDeniedBlocksExecutionAndAudits(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	decisions := make(chan ApprovalDecision, 1)
	sawApproval := false
	_, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session:     session,
		Tool:        "run_shell",
		Input:       map[string]any{"command": "touch denied-marker.txt"},
		Posture:     PostureManual,
		Interactive: true,
		Decisions:   decisions,
		Emit: func(ev StreamEvent) {
			if ev.Kind == StreamApproval && ev.Approval != nil {
				sawApproval = true
				decisions <- ApprovalDecision{ID: ev.Approval.ID, Approved: false, Reason: "test deny"}
			}
		},
	})
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	if !sawApproval {
		t.Fatal("expected a StreamApproval before denial")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "denied-marker.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("denied shell command must not execute")
	}
	calls, results := transcriptToolEntries(t, session.TranscriptPath, "run_shell")
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("transcript entries: %d call(s), %d result(s); want 1 and 1", len(calls), len(results))
	}
	if _, ok := results[0].Output["error"]; !ok {
		t.Fatalf("denied tool_result must carry an error: %+v", results[0].Output)
	}
	records := readToolCallRecords(t, session.ToolCallsPath)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want exactly 1", len(records))
	}
	if _, ok := records[0]["error"]; !ok {
		t.Fatalf("denied audit record must carry an error: %+v", records[0])
	}
}

// A gated tool without an operator attached fails closed and is still audited.
func TestExecutorHeadlessApprovalFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	_, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "transfer_worker",
		Input:   map[string]any{"env": "staging"},
		Posture: PostureAutoSafe, // RequiresApproval floor must still prompt
	})
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want ErrToolDenied", err)
	}
	_, results := transcriptToolEntries(t, session.TranscriptPath, "transfer_worker")
	if len(results) != 1 {
		t.Fatalf("tool_result entries = %d, want 1", len(results))
	}
	msg, _ := results[0].Output["error"].(string)
	if !strings.Contains(msg, "headless") {
		t.Fatalf("headless denial reason missing: %q", msg)
	}
	records := readToolCallRecords(t, session.ToolCallsPath)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want exactly 1", len(records))
	}
}

// A successful governed call persists tool_call, tool_result, and one audit
// record; write_worker_file actually lands inside the sandbox.
func TestExecutorSuccessWritesTranscriptAndAudit(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	result, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "write_worker_file",
		Input:   map[string]any{"path": "notes.txt", "content": "hello governed world\n"},
		Posture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(result.Summary, "notes.txt") {
		t.Fatalf("summary = %q", result.Summary)
	}
	data, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || !strings.Contains(string(data), "hello governed world") {
		t.Fatalf("file not written: %v %q", err, data)
	}
	calls, results := transcriptToolEntries(t, session.TranscriptPath, "write_worker_file")
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("transcript entries: %d call(s), %d result(s); want 1 and 1", len(calls), len(results))
	}
	records := readToolCallRecords(t, session.ToolCallsPath)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want exactly 1", len(records))
	}
	if _, ok := records[0]["error"]; ok {
		t.Fatalf("successful audit record must not carry an error: %+v", records[0])
	}
}

// A failing tool (path outside the sandbox) is still fully audited.
func TestExecutorFailureIsAudited(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	_, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "write_worker_file",
		Input:   map[string]any{"path": "../escape.txt", "content": "nope"},
		Posture: PostureAutoSafe,
	})
	if err == nil || errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want a sandbox execution error", err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("write escaped the worker sandbox")
	}
	_, results := transcriptToolEntries(t, session.TranscriptPath, "write_worker_file")
	if len(results) != 1 {
		t.Fatalf("tool_result entries = %d, want 1", len(results))
	}
	if _, ok := results[0].Output["error"]; !ok {
		t.Fatalf("failed tool_result must carry an error: %+v", results[0].Output)
	}
	records := readToolCallRecords(t, session.ToolCallsPath)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want exactly 1", len(records))
	}
	if _, ok := records[0]["error"]; !ok {
		t.Fatalf("failed audit record must carry an error: %+v", records[0])
	}
}

// Secrets are redacted from both the transcript and the tool-call audit log.
func TestExecutorRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	const secret = "sk-executor-secret-token"
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Redactor: NewRedactor(secret)})

	_, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "write_worker_file",
		Input:   map[string]any{"path": "config.txt", "content": "token=" + secret + "\n"},
		Posture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, path := range []string{session.TranscriptPath, session.ToolCallsPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s leaked the secret", filepath.Base(path))
		}
	}
}

// run_shell executes inside the selected worker directory.
func TestExecutorRunShellRunsInWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	result, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "run_shell",
		Input:   map[string]any{"command": "cat pip.yaml"},
		Posture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out, _ := result.Data["output"].(string)
	if !strings.Contains(out, "name: demo") {
		t.Fatalf("run_shell output = %q, want pip.yaml content", out)
	}
}

// An unknown tool never runs but still yields transcript + audit records.
func TestExecutorUnknownToolIsAudited(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	_, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "no_such_tool",
		Posture: PostureAutoSafe,
	})
	if err == nil || errors.Is(err, ErrToolDenied) {
		t.Fatalf("err = %v, want unknown-tool error", err)
	}
	records := readToolCallRecords(t, session.ToolCallsPath)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want exactly 1", len(records))
	}
}

// Operator-only tools must not be exposed to the model tool-calling loop.
func TestOperatorOnlyToolsHiddenFromModel(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, _ := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	for _, spec := range rt.toolSpecs() {
		if spec.Name == "run_shell" || spec.Name == "write_worker_file" {
			t.Fatalf("operator-only tool %q leaked into the model tool specs", spec.Name)
		}
	}
	// They remain registered for the governed executor.
	if _, ok := rt.Tools.Tool("run_shell"); !ok {
		t.Fatal("run_shell not registered")
	}
	if _, ok := rt.Tools.Tool("write_worker_file"); !ok {
		t.Fatal("write_worker_file not registered")
	}
}
