package operate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type interruptedMutationModel struct{ calls int }

func (m *interruptedMutationModel) Complete(_ context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.calls++
	switch m.calls {
	case 1:
		return provider.Response{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{
			ID: "durable-write", Name: "write_worker_file",
			Arguments: json.RawMessage(`{"path":"durable.go","content":"package main\n\nconst durableMutation = true\n"}`),
		}}}, nil
	case 2:
		return provider.Response{}, errors.New("simulated transport interruption")
	default:
		return provider.Response{Text: "Everything is done.", StopReason: provider.StopEndTurn}, nil
	}
}

type mutateAfterBuildModel struct {
	dir   string
	calls int
}

func (m *mutateAfterBuildModel) Complete(_ context.Context, _ provider.Request, _ func(string)) (provider.Response, error) {
	m.calls++
	switch m.calls {
	case 1:
		return provider.Response{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{
			ID: "write", Name: "write_worker_file",
			Arguments: json.RawMessage(`{"path":"proof.go","content":"package main\n\nconst proof = true\n"}`),
		}}}, nil
	case 2:
		return provider.Response{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{
			ID: "audit", Name: "audit_worker", Arguments: json.RawMessage(`{}`),
		}}}, nil
	case 3:
		return provider.Response{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{
			ID: "build", Name: "build_worker", Arguments: json.RawMessage(`{}`),
		}}}, nil
	case 4:
		if err := os.WriteFile(filepath.Join(m.dir, "proof.go"), []byte("package main\n\nconst proof = false\n"), 0o644); err != nil {
			return provider.Response{}, err
		}
		return provider.Response{Text: "verified and complete", StopReason: provider.StopEndTurn}, nil
	default:
		return provider.Response{}, errors.New("stale completion correctly requested another model turn")
	}
}

func TestAgentLoopRevalidatesPersistedEvidenceImmediatelyBeforeSuccess(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	buildCalls := 0
	harness := newEvidenceHarness(t, dir, &buildCalls, nil)
	model := &mutateAfterBuildModel{dir: dir}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Harness: harness, Model: model, ModelID: "test/model",
		HeadlessPosture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "modify and verify the worker")
	if err == nil || !strings.Contains(err.Error(), "stale completion correctly requested") {
		t.Fatalf("Prompt() error = %v, want stale evidence to force another turn", err)
	}
	if turn.Final == "verified and complete" {
		t.Fatal("source mutation after build bypassed the final persisted-evidence check")
	}
	entries, readErr := ReadTranscript(started.Session.TranscriptPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	found := false
	for _, entry := range entries {
		if entry.Kind == TranscriptStatus && metaString(entry.Metadata, "completion_gate") == "incomplete" {
			found = true
		}
	}
	if !found {
		t.Fatal("stale source produced no completion evidence request")
	}
}

func TestCompletionGateSurvivesInterruptedTurnAndLaterPrompt(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &interruptedMutationModel{}
	runtime, err := NewAgentRuntime(RuntimeOptions{
		Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model",
		HeadlessPosture: PostureAutoSafe,
	})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if _, err := runtime.Prompt(context.Background(), started.Session.ID, "modify the worker"); err == nil || !strings.Contains(err.Error(), "simulated transport interruption") {
		t.Fatalf("first Prompt() error = %v, want simulated interruption", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "durable.go")); err != nil {
		t.Fatalf("mutation did not execute before interruption: %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "report status")
	if !errors.Is(err, ErrAgentLoopExhausted) {
		t.Fatalf("second Prompt() error = %v, want durable incomplete gate", err)
	}
	if turn.Final == "Everything is done." {
		t.Fatal("later model text bypassed the durable completion gate")
	}
	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Kind == TranscriptStatus && metaString(entry.Metadata, "completion_gate") == "incomplete" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("durable session transcript has no completion evidence request")
	}
}

func TestAgentLoopMutationRequiresObservedCompletionEvidence(t *testing.T) {
	validSHA := strings.Repeat("a", 64)
	sourceSHA := strings.Repeat("b", 64)
	auditSHA := strings.Repeat("c", 64)
	tests := []struct {
		name       string
		mutation   string
		arguments  json.RawMessage
		toolResult ToolResult
	}{
		{
			name:      "scaffold state",
			mutation:  "scaffold_worker",
			arguments: json.RawMessage(`{"name":"demo","trigger":"POST /tickets"}`),
			toolResult: ToolResult{Summary: "scaffolded", Data: map[string]any{
				"name": "demo", "dir": "/tmp/demo",
			}},
		},
		{
			name:      "patch diff",
			mutation:  "patch_worker",
			arguments: json.RawMessage(`{"goal":"add triage"}`),
			toolResult: ToolResult{Summary: "patched", Data: map[string]any{
				"changed_files": []string{"main.go"}, "diff_path": "/session/diff.patch",
			}},
		},
		{
			name:      "fix diff",
			mutation:  "fix_worker",
			arguments: json.RawMessage(`{"subject":"repair audit"}`),
			toolResult: ToolResult{Summary: "fixed", Data: map[string]any{
				"changed_files": []string{"main.go"}, "diff_path": "/session/diff.patch",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeMinimalWorker(t, dir)
			var executed []string
			registry := completionToolRegistry(map[string]func() ToolResult{
				tt.mutation: func() ToolResult { return tt.toolResult },
				"audit_worker": func() ToolResult {
					return ToolResult{Summary: "audit passed", Data: map[string]any{
						"passed": true, "audit_path": "/session/audit.json", "source_sha256": sourceSHA, "audit_sha256": auditSHA,
					}}
				},
				"build_worker": func() ToolResult {
					return ToolResult{Summary: "built", Data: map[string]any{
						"binary_path": "/worker/bin/demo", "sha256": validSHA, "source_sha256": sourceSHA,
						"audit_sha256": auditSHA, "audit_passed": true,
					}}
				},
			}, &executed)
			model := &scriptedModel{steps: []provider.Response{
				{
					StopReason: provider.StopToolUse,
					ToolCalls:  []provider.ToolCall{{ID: "mutation", Name: tt.mutation, Arguments: tt.arguments}},
				},
				{
					Text:       "Everything is complete: audit passed and sha256=" + validSHA,
					StopReason: provider.StopEndTurn,
				},
				{
					StopReason: provider.StopToolUse,
					ToolCalls: []provider.ToolCall{
						{ID: "audit", Name: "audit_worker", Arguments: json.RawMessage(`{}`)},
						{ID: "build", Name: "build_worker", Arguments: json.RawMessage(`{}`)},
					},
				},
				{Text: "Worker verified and built.", StopReason: provider.StopEndTurn},
			}}
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

			turn, err := runtime.Prompt(context.Background(), started.Session.ID, "construct the worker")
			if err != nil {
				t.Fatalf("Prompt() error = %v", err)
			}
			if turn.Final != "Worker verified and built." {
				t.Fatalf("turn.Final = %q, want verified final", turn.Final)
			}
			if model.i != 4 {
				t.Fatalf("model calls = %d, want 4; premature text must not complete", model.i)
			}
			if got := strings.Join(executed, ","); got != tt.mutation+",audit_worker,build_worker" {
				t.Fatalf("executed tools = %q", got)
			}

			request := lastMessageText(t, model.requests[2])
			var payload struct {
				Type            string   `json:"type"`
				Outcome         string   `json:"outcome"`
				MissingEvidence []string `json:"missing_evidence"`
				RequiredTools   []string `json:"required_tools"`
			}
			if err := json.Unmarshal([]byte(request), &payload); err != nil {
				t.Fatalf("completion request is not structured JSON: %v\n%s", err, request)
			}
			if payload.Type != "ouvrier_completion_evidence_request" || payload.Outcome != "incomplete" {
				t.Fatalf("completion request = %+v", payload)
			}
			if got := strings.Join(payload.MissingEvidence, ","); got != "passing_audit,build_artifact_sha256" {
				t.Fatalf("missing evidence = %q", got)
			}
			if got := strings.Join(payload.RequiredTools, ","); got != "audit_worker,build_worker" {
				t.Fatalf("required tools = %q", got)
			}
		})
	}
}

func TestAgentLoopRequiresValidSHA256FromExecutedBuild(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	validSHA := strings.Repeat("b", 64)
	sourceSHA := strings.Repeat("c", 64)
	auditSHA := strings.Repeat("d", 64)
	buildCalls := 0
	registry := completionToolRegistry(map[string]func() ToolResult{
		"patch_worker": func() ToolResult {
			return ToolResult{Summary: "patched", Data: map[string]any{
				"changed_files": []string{"main.go"}, "diff_path": "/session/diff.patch",
			}}
		},
		"audit_worker": func() ToolResult {
			return ToolResult{Summary: "audit passed", Data: map[string]any{
				"passed": true, "audit_path": "/session/audit.json", "source_sha256": sourceSHA, "audit_sha256": auditSHA,
			}}
		},
		"build_worker": func() ToolResult {
			buildCalls++
			sha := "not-a-sha256"
			if buildCalls == 2 {
				sha = validSHA
			}
			return ToolResult{Summary: "built", Data: map[string]any{
				"binary_path": "/worker/bin/demo", "sha256": sha, "source_sha256": sourceSHA,
				"audit_sha256": auditSHA, "audit_passed": true,
			}}
		},
	}, nil)
	model := &scriptedModel{steps: []provider.Response{
		{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "patch", Name: "patch_worker", Arguments: json.RawMessage(`{"goal":"change"}`)}}},
		{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{
			{ID: "audit", Name: "audit_worker", Arguments: json.RawMessage(`{}`)},
			{ID: "bad-build", Name: "build_worker", Arguments: json.RawMessage(`{}`)},
		}},
		{Text: "Done with sha256=not-a-sha256", StopReason: provider.StopEndTurn},
		{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "good-build", Name: "build_worker", Arguments: json.RawMessage(`{}`)}}},
		{Text: "Verified build complete.", StopReason: provider.StopEndTurn},
	}}
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "repair and build")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if turn.Final != "Verified build complete." || buildCalls != 2 || model.i != 5 {
		t.Fatalf("turn/build/model = %q/%d/%d, invalid SHA must not complete", turn.Final, buildCalls, model.i)
	}
	request := lastMessageText(t, model.requests[3])
	if !strings.Contains(request, `"missing_evidence":["build_artifact_sha256"]`) {
		t.Fatalf("completion request = %s, want missing build SHA evidence", request)
	}
}

func TestAgentLoopReadOnlyInspectionCanFinishOnText(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	registry := completionToolRegistry(map[string]func() ToolResult{
		"review_worker": func() ToolResult {
			return ToolResult{Summary: "reviewed", Data: map[string]any{"summary": "no findings"}}
		},
	}, nil)
	model := &scriptedModel{steps: []provider.Response{
		{StopReason: provider.StopToolUse, ToolCalls: []provider.ToolCall{{ID: "review", Name: "review_worker", Arguments: json.RawMessage(`{}`)}}},
		{Text: "Inspection complete: no findings.", StopReason: provider.StopEndTurn},
	}}
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "inspect the worker")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if turn.Final != "Inspection complete: no findings." || model.i != 2 {
		t.Fatalf("turn/model = %q/%d", turn.Final, model.i)
	}
}

func TestAgentLoopStepExhaustionIsExplicitFailure(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	registry := completionToolRegistry(map[string]func() ToolResult{
		"list_workers": func() ToolResult { return ToolResult{Summary: "listed"} },
	}, nil)
	steps := make([]provider.Response, maxAgentSteps)
	for i := range steps {
		steps[i] = provider.Response{
			StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{
				ID: "list-" + string(rune('a'+i)), Name: "list_workers", Arguments: json.RawMessage(`{}`),
			}},
		}
	}
	model := &scriptedModel{steps: steps}
	runtime, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: registry})
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	started, err := runtime.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	turn, err := runtime.Prompt(context.Background(), started.Session.ID, "keep inspecting forever")
	if err == nil {
		t.Fatal("Prompt() error = nil, want exhaustion failure")
	}
	if !errors.Is(err, ErrAgentLoopExhausted) {
		t.Fatalf("Prompt() error = %v, want ErrAgentLoopExhausted", err)
	}
	if turn.Outcome != RuntimeOutcomeExhausted {
		t.Fatalf("turn.Outcome = %q, want %q", turn.Outcome, RuntimeOutcomeExhausted)
	}
	if !strings.Contains(turn.Final, "outcome exhausted") || model.i != maxAgentSteps {
		t.Fatalf("turn/model = %q/%d", turn.Final, model.i)
	}

	entries, err := ReadTranscript(started.Session.TranscriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	last := entries[len(entries)-1]
	if last.Kind != TranscriptError || last.Metadata["outcome"] != string(RuntimeOutcomeExhausted) {
		t.Fatalf("last transcript entry = %+v", last)
	}
}

func TestCompletionGateInvalidatesProofAfterLaterMutation(t *testing.T) {
	gate := agentCompletionGate{}
	sourceSHA := strings.Repeat("a", 64)
	auditSHA := strings.Repeat("b", 64)
	gate.observe("patch_worker", ToolResult{Data: map[string]any{
		"changed_files": []string{"main.go"}, "diff_path": "/session/first.diff",
	}}, nil)
	gate.observe("audit_worker", ToolResult{Data: map[string]any{
		"passed": true, "audit_path": "/session/audit.json", "source_sha256": sourceSHA, "audit_sha256": auditSHA,
	}}, nil)
	gate.observe("build_worker", ToolResult{Data: map[string]any{
		"binary_path": "/worker/bin/demo", "sha256": strings.Repeat("c", 64), "source_sha256": sourceSHA,
		"audit_sha256": auditSHA, "audit_passed": true,
	}}, nil)
	if !gate.complete() {
		t.Fatal("gate is incomplete after observed mutation, passing audit, and valid build")
	}

	gate.observe("fix_worker", ToolResult{Data: map[string]any{
		"changed_files": []string{"tool.go"}, "diff_path": "/session/second.diff",
	}}, nil)
	if gate.complete() {
		t.Fatal("later mutation retained stale audit/build proof")
	}
	if got := strings.Join(gate.missingEvidence(), ","); got != "passing_audit,build_artifact_sha256" {
		t.Fatalf("missing evidence after later mutation = %q", got)
	}
}

func TestCompletionGateRequiresFreshAuditAndBuildAfterGovernedWrite(t *testing.T) {
	gate := agentCompletionGate{}
	gate.observe("write_worker_file", ToolResult{Data: map[string]any{
		"path": "main.go", "bytes": 42, "changed": true, "source_sha256": strings.Repeat("a", 64),
	}}, nil)
	if gate.complete() {
		t.Fatal("gate completed after write without audit and build")
	}
	if got := strings.Join(gate.missingEvidence(), ","); got != "passing_audit,build_artifact_sha256" {
		t.Fatalf("missing evidence = %q", got)
	}

	noOp := agentCompletionGate{}
	noOp.observe("write_worker_file", ToolResult{Data: map[string]any{
		"path": "main.go", "bytes": 42, "changed": false, "source_sha256": strings.Repeat("a", 64),
	}}, nil)
	if !noOp.complete() {
		t.Fatal("unchanged write incorrectly activated completion gate")
	}
}

func TestCompletionGateNeverTreatsExistingDiffAsMutationProof(t *testing.T) {
	gate := agentCompletionGate{}
	gate.requireMutationIntent()
	gate.observe("diff_worker", ToolResult{Data: map[string]any{
		"status": "not a Git worktree",
	}}, nil)
	if gate.workerObserved || gate.complete() {
		t.Fatal("non-Git status satisfied mutation proof")
	}
	gate.observe("diff_worker", ToolResult{Data: map[string]any{
		"status": " M main.go", "changed_files": []string{"main.go"}, "diff": "pre-existing diff",
	}}, nil)
	if gate.workerObserved || gate.complete() {
		t.Fatal("pre-existing candidate diff satisfied mutation proof")
	}
	request, err := gate.request()
	if err != nil {
		t.Fatalf("request() error = %v", err)
	}
	if !strings.Contains(request, `"required_tools":["write_worker_file"`) {
		t.Fatalf("completion request = %s, want an actual mutation tool", request)
	}
}

func TestCompletionGateRejectsBuildNotBoundToObservedAudit(t *testing.T) {
	gate := agentCompletionGate{}
	sourceSHA := strings.Repeat("a", 64)
	auditSHA := strings.Repeat("b", 64)
	gate.observe("patch_worker", ToolResult{Data: map[string]any{
		"changed_files": []string{"main.go"}, "diff_path": "/session/diff.patch",
	}}, nil)
	gate.observe("audit_worker", ToolResult{Data: map[string]any{
		"passed": true, "audit_path": "/session/audit.json", "source_sha256": sourceSHA, "audit_sha256": auditSHA,
	}}, nil)
	gate.observe("build_worker", ToolResult{Data: map[string]any{
		"binary_path": "/worker/bin/demo", "sha256": strings.Repeat("c", 64),
		"source_sha256": strings.Repeat("d", 64), "audit_sha256": auditSHA, "audit_passed": true,
	}}, nil)
	if gate.complete() {
		t.Fatal("gate accepted a build produced from a different source fingerprint")
	}
	if got := strings.Join(gate.missingEvidence(), ","); got != "build_artifact_sha256" {
		t.Fatalf("missing evidence = %q, want build artifact", got)
	}

	gate.observe("build_worker", ToolResult{Data: map[string]any{
		"binary_path": "/worker/bin/demo", "sha256": strings.Repeat("c", 64),
		"source_sha256": sourceSHA, "audit_sha256": strings.Repeat("e", 64), "audit_passed": true,
	}}, nil)
	if gate.complete() {
		t.Fatal("gate accepted a build bound to a different audit document")
	}

	gate.observe("build_worker", ToolResult{Data: map[string]any{
		"binary_path": "/worker/bin/demo", "sha256": strings.Repeat("c", 64),
		"source_sha256": sourceSHA, "audit_sha256": auditSHA, "audit_passed": false,
	}}, nil)
	if gate.complete() {
		t.Fatal("gate accepted an operator-override build as verified completion evidence")
	}
}

func TestHistoryMessagesReplaysPersistedCompletionRequest(t *testing.T) {
	request := `{"type":"ouvrier_completion_evidence_request","outcome":"incomplete","missing_evidence":["passing_audit"]}`
	entries := []TranscriptEntry{
		{Kind: TranscriptUser, Text: "repair the worker"},
		{Kind: TranscriptToolCall, ToolName: "patch_worker", Input: map[string]any{"goal": "repair"}, Metadata: map[string]any{"tool_call_id": "patch"}},
		{Kind: TranscriptToolResult, ToolName: "patch_worker", Output: map[string]any{"changed_files": []any{"main.go"}}, Metadata: map[string]any{"tool_call_id": "patch"}},
		{Kind: TranscriptAssistant, Text: "done"},
		{Kind: TranscriptStatus, Text: request, Metadata: map[string]any{"completion_gate": "incomplete"}},
		{Kind: TranscriptToolCall, ToolName: "audit_worker", Input: map[string]any{}, Metadata: map[string]any{"tool_call_id": "audit"}},
		{Kind: TranscriptToolResult, ToolName: "audit_worker", Output: map[string]any{"passed": true}, Metadata: map[string]any{"tool_call_id": "audit"}},
	}

	messages, err := historyMessages(entries)
	if err != nil {
		t.Fatalf("historyMessages() error = %v", err)
	}
	var requestIndex = -1
	for i, message := range messages {
		if err := message.Validate(); err != nil {
			t.Fatalf("message %d invalid: %v", i, err)
		}
		if message.Role == provider.RoleUser && message.Text() == request {
			requestIndex = i
		}
	}
	if requestIndex < 1 || requestIndex+1 >= len(messages) {
		t.Fatalf("completion request missing from replay: %+v", messages)
	}
	if messages[requestIndex-1].Role != provider.RoleAssistant || messages[requestIndex-1].Text() != "done" {
		t.Fatalf("message before completion request = %+v", messages[requestIndex-1])
	}
	if messages[requestIndex+1].Role != provider.RoleAssistant {
		t.Fatalf("message after completion request = %+v, want assistant tool call", messages[requestIndex+1])
	}
}

func completionToolRegistry(results map[string]func() ToolResult, executed *[]string) *ToolRegistry {
	registry := &ToolRegistry{tools: map[string]Tool{}}
	for name, result := range results {
		name, result := name, result
		registry.Register(Tool{
			Name:       name,
			Governance: GovReadOnly,
			Run: func(_ context.Context, env ToolEnv, _ map[string]any) (ToolResult, error) {
				if executed != nil {
					*executed = append(*executed, name)
				}
				value := result()
				return materializeCompletionTestEvidence(env, name, value)
			},
		})
	}
	return registry
}

// materializeCompletionTestEvidence keeps the loop tests honest: a structured
// fake tool result is not enough to pass the production completion gate. When
// a fake declares valid audit/build evidence, create the corresponding
// source-bound artifacts so the last-moment verifier can recompute them.
func materializeCompletionTestEvidence(env ToolEnv, name string, result ToolResult) (ToolResult, error) {
	if env.Session == nil {
		return result, nil
	}
	switch name {
	case "audit_worker":
		passed, _ := result.Data["passed"].(bool)
		if !passed {
			return result, nil
		}
		snapshot, err := stableCandidateSourceSnapshot(env.Session.Dir)
		if err != nil {
			return ToolResult{}, err
		}
		report := AuditReport{
			Workspace: snapshot.Workspace, SourceSHA256: snapshot.SHA256,
			SourceFiles: snapshot.Files, SourceBytes: snapshot.Bytes,
			Toolchain: snapshot.Toolchain, LocalReplacements: snapshot.LocalReplacements,
			Passed: true,
		}
		if err := WriteAuditReport(env.Session.AuditPath, report); err != nil {
			return ToolResult{}, err
		}
		evidence, err := CurrentAuditEvidence(env.Session.AuditPath, env.Session.Dir)
		if err != nil {
			return ToolResult{}, err
		}
		result.Data["audit_path"] = env.Session.AuditPath
		result.Data["source_sha256"] = snapshot.SHA256
		result.Data["audit_sha256"] = evidence.ArtifactSHA256
	case "build_worker":
		auditPassed, _ := result.Data["audit_passed"].(bool)
		if !auditPassed || !isSHA256(resultDataString(result.Data, "sha256")) {
			return result, nil
		}
		evidence, err := CurrentAuditEvidence(env.Session.AuditPath, env.Session.Dir)
		if err != nil {
			return ToolResult{}, err
		}
		binaryPath := filepath.Join(filepath.Dir(env.Session.BuildPath), "completion-test-worker")
		if err := os.WriteFile(binaryPath, []byte("verified test artifact"), 0o755); err != nil {
			return ToolResult{}, err
		}
		binarySHA, err := fileSHA256(binaryPath)
		if err != nil {
			return ToolResult{}, err
		}
		report := evidence.Report
		artifact := BuildArtifact{
			SessionID: env.Session.ID, Workspace: report.Workspace,
			SourceSHA256: report.SourceSHA256, SourceFiles: report.SourceFiles,
			SourceBytes: report.SourceBytes, Toolchain: report.Toolchain,
			LocalReplacements: report.LocalReplacements, AuditPath: env.Session.AuditPath,
			AuditSHA256: evidence.ArtifactSHA256, BinaryPath: binaryPath,
			SHA256: binarySHA, AuditPassed: true,
		}
		if err := WriteBuildArtifact(env.Session.BuildPath, artifact); err != nil {
			return ToolResult{}, err
		}
		result.Data["binary_path"] = binaryPath
		result.Data["sha256"] = binarySHA
		result.Data["source_sha256"] = report.SourceSHA256
		result.Data["audit_sha256"] = evidence.ArtifactSHA256
	}
	return result, nil
}

func lastMessageText(t *testing.T, req provider.Request) string {
	t.Helper()
	if len(req.Messages) == 0 {
		t.Fatal("provider request has no messages")
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != provider.RoleUser {
		t.Fatalf("last message role = %q, want user completion request", last.Role)
	}
	return last.Text()
}
