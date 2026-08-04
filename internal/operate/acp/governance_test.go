package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestPromptWithContextIsBoundedAndRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := operate.TurnRequest{
		CWD: dir, Prompt: "update the worker", ContextFiles: []string{"main.go"},
		OutputSchema: `{"type":"object"}`,
	}
	prompt, err := promptWithContext(req)
	if err != nil {
		t.Fatalf("promptWithContext() error = %v", err)
	}
	for _, want := range []string{"update the worker", "package main", "main.go", `{"type":"object"}`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	req.ContextFiles = []string{"../secret"}
	if _, err := promptWithContext(req); err == nil {
		t.Fatal("promptWithContext() accepted an escaping context path")
	}
}

func TestPermissionPolicyRejectsAgentFilesystemMutation(t *testing.T) {
	dir := t.TempDir()
	client := &client{req: operate.TurnRequest{CWD: dir, Sandbox: operate.SandboxWorkspaceWrite}}
	if client.permissionAllowed("edit", json.RawMessage(`{"file_path":"/etc/passwd"}`)) {
		t.Fatal("permission policy allowed an external edit")
	}
	if client.permissionAllowed("edit", json.RawMessage(`{"file_path":"main.go","old_string":"a","new_string":"b"}`)) {
		t.Fatal("permission policy let the agent mutate even the disposable stage directly")
	}
	if client.permissionAllowed("execute", json.RawMessage(`{"command":"go test ./..."}`)) {
		t.Fatal("permission policy allowed command execution")
	}
	if client.permissionAllowed("think", nil) {
		t.Fatal("permission policy allowed an ungoverned thinking/subagent tool")
	}
}

func TestSafeACPModePrefersCodexReadOnlyAndClaudeDefault(t *testing.T) {
	readOnly, ok := safeACPModeValue(sessionConfigOption{
		ID: "mode", CurrentValue: "agent",
		Options: []sessionConfigChoice{{Value: "agent"}, {Value: "read-only"}, {Value: "agent-full-access"}},
	})
	if !ok || readOnly != "read-only" {
		t.Fatalf("Codex safe mode = %v, %v", readOnly, ok)
	}
	defaultMode, ok := safeACPModeValue(sessionConfigOption{
		ID: "mode", CurrentValue: "bypassPermissions",
		Options: []sessionConfigChoice{{Value: "default"}, {Value: "bypassPermissions"}},
	})
	if !ok || defaultMode != "default" {
		t.Fatalf("Claude safe mode = %v, %v", defaultMode, ok)
	}
}

func TestApplyPatchPlanValidatesEveryFileBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(patchPlan{Summary: "oversized", Files: []patchPlanFile{
		{Path: "first.go", Content: "package first\n"},
		{Path: "too-large.go", Content: strings.Repeat("x", maxPatchFileBytes+1)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyPatchPlan(operate.TurnRequest{CWD: dir}, operate.TurnResult{FinalMessage: string(raw)}, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("applyPatchPlan() error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "first.go")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("patch plan wrote before complete validation: %v", statErr)
	}
}

func TestApplyPatchPlanAcceptsCanonicalToollessFullFileEnvelope(t *testing.T) {
	dir := t.TempDir()
	content := "package main\n\nfunc main() {}\n"
	record, err := json.Marshal(map[string]string{
		"file_path": filepath.Join(dir, "main.go"),
		"content":   content,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := applyPatchPlan(
		operate.TurnRequest{CWD: dir},
		operate.TurnResult{FinalMessage: "* " + string(record)}, nil,
	)
	if err != nil {
		t.Fatalf("applyPatchPlan() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil || string(data) != content {
		t.Fatalf("canonical full-file envelope was not applied: %q, %v", data, err)
	}
	if !strings.Contains(result.FinalMessage, `"changed_files":["main.go"]`) {
		t.Fatalf("normalized result = %q", result.FinalMessage)
	}
}

func TestApplyPatchPlanRejectsCanonicalEnvelopeOutsideStage(t *testing.T) {
	dir := t.TempDir()
	record, err := json.Marshal(map[string]string{
		"file_path": filepath.Join(filepath.Dir(dir), "outside.go"),
		"content":   "package outside\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyPatchPlan(
		operate.TurnRequest{CWD: dir},
		operate.TurnResult{FinalMessage: "* " + string(record)}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("applyPatchPlan() error = %v", err)
	}
}
