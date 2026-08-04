package operate

import (
	"context"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// A slash command must be handled by the planner/tools, never sent to the model,
// even when a model transport is configured.
func TestSlashCommandBypassesModel(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{{Text: "should not be called", StopReason: provider.StopEndTurn}}}
	rt, err := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	turn, err := rt.Prompt(context.Background(), started.Session.ID, "/help")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if model.i != 0 {
		t.Fatalf("model was called %d time(s) for a slash command; it must be bypassed", model.i)
	}
	if turn.Final == "" {
		t.Fatal("expected /help to produce assistant help text")
	}
}

func TestClaudeLoginSlashDelegatesToClaudeCLI(t *testing.T) {
	runtime := &AgentRuntime{Tools: NewToolRegistry()}
	plan := runtime.planSlash("/login claude")
	if len(plan.Tools) != 0 || !strings.Contains(plan.Assistant, "detected during cockpit startup") || strings.Contains(plan.Assistant, " auth login") {
		t.Fatalf("agent readiness plan = %+v", plan)
	}
}

// Free-form natural language still reaches the model loop.
func TestFreeTextReachesModel(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{{Text: "hi from model", StopReason: provider.StopEndTurn}}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	if _, err := rt.Prompt(context.Background(), started.Session.ID, "say hi"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if model.i == 0 {
		t.Fatal("free text should reach the model loop")
	}
}
