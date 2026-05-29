package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// approvalGateAdapter bridges the durable state.Store to the tools.ApprovalGate
// contract so that gated tool calls record a pending approval and suspend the
// run instead of being hard-denied.
type approvalGateAdapter struct {
	store state.Store
}

func (a approvalGateAdapter) RecordPendingApproval(ctx context.Context, req tools.ApprovalRequest) (string, error) {
	id := newApprovalID()
	if err := a.store.SaveApproval(ctx, state.PendingApproval{
		ID:         id,
		ExecID:     req.ExecID,
		SessionID:  req.SessionID,
		TraceID:    req.TraceID,
		ToolName:   req.ToolName,
		ToolCallID: req.ToolCallID,
		ToolKind:   req.ToolKind,
		Effect:     req.Effect,
		Reason:     req.Reason,
		Status:     state.ApprovalPending,
	}); err != nil {
		return "", err
	}
	return id, nil
}

func newApprovalID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("appr_%d", time.Now().UnixNano())
	}
	return "appr_" + hex.EncodeToString(random)
}
