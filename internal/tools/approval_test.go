package tools

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type fakeApprovalGate struct {
	mu        sync.Mutex
	recorded  []ApprovalRequest
	returnID  string
	returnErr error
}

func (g *fakeApprovalGate) RecordPendingApproval(ctx context.Context, req ApprovalRequest) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recorded = append(g.recorded, req)
	if g.returnErr != nil {
		return "", g.returnErr
	}
	id := g.returnID
	if id == "" {
		id = "approval-1"
	}
	return id, nil
}

func newApprovalExecutor(t *testing.T) *Executor {
	t.Helper()
	executor := NewExecutor()
	if err := executor.Register("wire_payment", func(ctx context.Context) error {
		t.Fatal("gated tool body executed before approval")
		return nil
	}, WithMetadata(Metadata{
		Effect:           policy.EffectSideEffecting,
		RequiresApproval: true,
		SideEffects:      []string{"payment"},
	})); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	return executor
}

func TestExecuteSuspendsWhenApprovalGateConfigured(t *testing.T) {
	executor := newApprovalExecutor(t)
	gate := &fakeApprovalGate{returnID: "appr-42"}
	ctx := ContextWithApprovalGate(context.Background(), gate, ApprovalContext{
		ExecID:    "exec-1",
		SessionID: "session-1",
		TraceID:   "trace-1",
	})

	_, err := executor.Execute(ctx, provider.ToolCall{ID: "call-1", Name: "wire_payment"})
	var suspended *SuspendedError
	if !errors.As(err, &suspended) {
		t.Fatalf("Execute error = %v, want *SuspendedError", err)
	}
	if suspended.ApprovalID != "appr-42" {
		t.Fatalf("SuspendedError ApprovalID = %q, want appr-42", suspended.ApprovalID)
	}
	if len(gate.recorded) != 1 {
		t.Fatalf("recorded approvals = %d, want 1", len(gate.recorded))
	}
	req := gate.recorded[0]
	if req.ToolName != "wire_payment" || req.ToolCallID != "call-1" || req.ExecID != "exec-1" {
		t.Fatalf("recorded approval = %+v, missing tool/exec context", req)
	}
}

func TestExecuteHardDeniesWhenNoApprovalGate(t *testing.T) {
	executor := newApprovalExecutor(t)
	result, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call-1", Name: "wire_payment"})
	if err != nil {
		t.Fatalf("Execute returned error = %v, want hard-deny error result", err)
	}
	if !result.IsError {
		t.Fatal("Execute result IsError = false, want denied error result")
	}
}
