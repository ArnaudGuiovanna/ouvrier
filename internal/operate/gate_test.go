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
	model := &scriptedModel{steps: []provider.Response{
		{Text: "scaffolding", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "patch_worker", Arguments: json.RawMessage(`{"goal":"x"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
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
		if ev.Kind == StreamToolEnd && ev.Entry != nil && ev.Entry.ToolName == "patch_worker" {
			sawToolEnd = true
		}
	}
	if approvalID == "" {
		t.Fatal("expected a StreamApproval for patch_worker")
	}
	if !sawToolEnd {
		t.Fatal("patch_worker did not execute after approval")
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
