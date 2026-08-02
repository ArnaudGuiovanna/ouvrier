package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestExecutorClosedApprovalChannelFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	t.Cleanup(func() { _ = rt.Close() })
	decisions := make(chan ApprovalDecision)
	close(decisions)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := rt.Executor().Execute(ctx, GovernedCall{
		Session: session, Tool: "run_shell", Input: map[string]any{"command": "touch must-not-run"},
		Posture: PostureManual, Interactive: true, Decisions: decisions,
	})
	if !errors.Is(err, ErrToolDenied) || !strings.Contains(err.Error(), "channel closed") {
		t.Fatalf("Execute() error = %v, want fail-closed channel error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "must-not-run")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tool executed after decision channel closed: %v", statErr)
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
	if changed, _ := result.Data["changed"].(bool); !changed {
		t.Fatalf("write result = %+v, want observed mutation", result.Data)
	}
	if sourceSHA, _ := result.Data["source_sha256"].(string); len(sourceSHA) != 64 {
		t.Fatalf("write source_sha256 = %q, want bound source fingerprint", sourceSHA)
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

// An action whose durable audit receipt cannot be written must report an
// explicit failure, even when the underlying side effect already happened.
func TestExecutorAuditWriteFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, session := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})

	blockedPath := filepath.Join(t.TempDir(), "tool-calls-as-directory")
	if err := os.Mkdir(blockedPath, 0o755); err != nil {
		t.Fatalf("create blocked audit path: %v", err)
	}
	session.ToolCallsPath = blockedPath

	_, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session,
		Tool:    "write_worker_file",
		Input:   map[string]any{"path": "audit-failure.txt", "content": "side effect\n"},
		Posture: PostureAutoSafe,
	})
	if err == nil || !strings.Contains(err.Error(), "persist tool-call audit") {
		t.Fatalf("err = %v, want explicit audit persistence failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "audit-failure.txt")); statErr != nil {
		t.Fatalf("expected underlying write to have happened: %v", statErr)
	}
	_, results := transcriptToolEntries(t, session.TranscriptPath, "write_worker_file")
	if len(results) != 1 {
		t.Fatalf("tool_result entries = %d, want 1", len(results))
	}
	if got, _ := results[0].Output["audit_error"].(string); !strings.Contains(got, "persist tool-call audit") {
		t.Fatalf("tool_result audit_error = %q, want explicit persistence failure", got)
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
	decisions := make(chan ApprovalDecision, 1)

	result, err := rt.Executor().Execute(context.Background(), GovernedCall{
		Session: session, Tool: "run_shell", Input: map[string]any{"command": "cat pip.yaml"},
		Posture: PostureManual, Interactive: true, Decisions: decisions,
		Emit: func(event StreamEvent) {
			if event.Kind == StreamApproval && event.Approval != nil {
				decisions <- ApprovalDecision{ID: event.Approval.ID, Approved: true}
			}
		},
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

// Direct file writes are the model's governed construction path. Nested coding
// drivers and the operator shell must stay hidden from the model tool loop.
func TestModelConstructionToolsUseGovernedFileWrites(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	rt, _ := startExecutorRuntime(t, RuntimeOptions{Dir: dir, Driver: ManualDriver{}})
	exposed := map[string]bool{}
	for _, spec := range rt.toolSpecs() {
		exposed[spec.Name] = true
	}
	if !exposed["write_worker_file"] {
		t.Fatal("governed write_worker_file is not exposed to the model")
	}
	for _, hidden := range []string{"run_shell", "patch_worker", "fix_worker"} {
		if exposed[hidden] {
			t.Fatalf("operator-only tool %q leaked into model tool specs", hidden)
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
