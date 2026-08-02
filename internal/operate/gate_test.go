package operate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestGateInteractiveApprove(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	tools, executed := gateSideEffectRegistry()
	model := &scriptedModel{steps: []provider.Response{
		{Text: "scaffolding", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "mutate_probe", Arguments: json.RawMessage(`{}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: tools})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})

	ch, decisions, err := rt.RunTurnInteractive(context.Background(), started.Session.ID, "patch it", "prompt", PostureManual)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var approvalID string
	sawToolEnd := false
	for ev := range ch {
		if ev.Kind == StreamApproval && ev.Approval != nil {
			approvalID = ev.Approval.ID
			decisions <- ApprovalDecision{ID: approvalID, Approved: true}
		}
		if ev.Kind == StreamToolEnd && ev.Entry != nil && ev.Entry.ToolName == "mutate_probe" {
			sawToolEnd = true
		}
	}
	if approvalID == "" {
		t.Fatal("expected a StreamApproval for side-effecting model tool")
	}
	if !sawToolEnd || !*executed {
		t.Fatal("side-effecting tool did not execute after approval")
	}
}

func TestGateHeadlessFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "deploying", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "transfer_worker", Arguments: json.RawMessage(`{"env":"staging"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})

	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "deploy staging", "prompt")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	denied := false
	for ev := range ch {
		if ev.Kind == StreamToolEnd && ev.Entry != nil && ev.Entry.ToolName == "transfer_worker" {
			if _, ok := ev.Entry.Output["error"]; ok {
				denied = true
			}
		}
	}
	if !denied {
		t.Fatal("transfer_worker must fail closed in headless mode")
	}
}

// auto-safe posture auto-runs a side-effecting tool without an approval event.
func TestGateAutoSafeSkipsSideEffecting(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	tools, executed := gateSideEffectRegistry()
	model := &scriptedModel{steps: []provider.Response{
		{Text: "patching", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "mutate_probe", Arguments: json.RawMessage(`{}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model", Tools: tools})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	ch, _, _ := rt.RunTurnInteractive(context.Background(), started.Session.ID, "patch", "prompt", PostureAutoSafe)
	for ev := range ch {
		if ev.Kind == StreamApproval {
			t.Fatal("auto-safe must not prompt for a side-effecting tool")
		}
	}
	if !*executed {
		t.Fatal("auto-safe did not execute the side-effecting tool")
	}
}

func gateSideEffectRegistry() (*ToolRegistry, *bool) {
	executed := false
	registry := &ToolRegistry{tools: map[string]Tool{}}
	registry.Register(Tool{
		Name:       "mutate_probe",
		Governance: GovSideEffecting,
		Run: func(context.Context, ToolEnv, map[string]any) (ToolResult, error) {
			executed = true
			return ToolResult{Summary: "mutation observed"}, nil
		},
	})
	return registry, &executed
}

// auto-safe still prompts for a RequiresApproval (deploy) tool, with Prod set.
func TestGateAutoSafeStillPromptsDeploy(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "deploying", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "transfer_worker", Arguments: json.RawMessage(`{"env":"prod"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	ch, decisions, _ := rt.RunTurnInteractive(context.Background(), started.Session.ID, "deploy prod", "prompt", PostureAutoSafe)
	sawProd := false
	for ev := range ch {
		if ev.Kind == StreamApproval && ev.Approval != nil {
			if ev.Approval.Prod {
				sawProd = true
			}
			decisions <- ApprovalDecision{ID: ev.Approval.ID, Approved: false, Reason: "test deny"}
		}
	}
	if !sawProd {
		t.Fatal("auto-safe must still prompt for prod deploy with Prod=true")
	}
}

func TestApprovalRequestNormalizesProductionEnvironment(t *testing.T) {
	input := normalizeGovernedToolInput("transfer_worker", map[string]any{"env": "  ProD  "}, RuntimeOptions{})
	req := approvalRequestFor(plannedTool{
		ID:    "transfer",
		Name:  "transfer_worker",
		Input: input,
	}, GovRequiresApproval, nil, nil)
	if !req.Prod {
		t.Fatal("whitespace/case-normalized production environment must require production confirmation")
	}
	if got := req.Details["env"]; got != "prod" {
		t.Fatalf("approval env detail = %#v, want canonical prod", got)
	}
}

func TestApprovalRequestUsesEffectiveProductionEnvironmentDefault(t *testing.T) {
	input := normalizeGovernedToolInput("transfer_worker", nil, RuntimeOptions{Env: " Production "})
	req := approvalRequestFor(plannedTool{
		ID:    "transfer",
		Name:  "transfer_worker",
		Input: input,
	}, GovRequiresApproval, nil, nil)
	if !req.Prod {
		t.Fatal("production environment inherited from runtime options must require production confirmation")
	}
	if got := req.Details["env"]; got != "production" {
		t.Fatalf("approval env detail = %#v, want canonical production", got)
	}
}

func TestTransferApprovalDisclosesAcceptedRiskOverride(t *testing.T) {
	session := &Session{AcceptedRiskReason: "operator accepted a failed review"}
	req := approvalRequestFor(plannedTool{
		ID:    "transfer",
		Name:  "transfer_worker",
		Input: map[string]any{"env": "staging"},
	}, GovRequiresApproval, nil, session)
	if got := req.Details["accepted_risk_override"]; got != session.AcceptedRiskReason {
		t.Fatalf("accepted risk detail = %#v, want explicit rationale", got)
	}
}
