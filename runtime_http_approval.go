package ovr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
	"github.com/ArnaudGuiovanna/ouvrier/internal/state"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tools"
)

// suspendedExecutionError reports whether err is a human-in-the-loop suspension
// raised by a gated tool call, returning the approval and execution ids so the
// runtime can answer with a 202 Accepted instead of a failure.
func suspendedExecutionError(err error) (*tools.SuspendedError, bool) {
	var suspended *tools.SuspendedError
	if errors.As(err, &suspended) {
		return suspended, true
	}
	return nil, false
}

type adminApprovalResponse struct {
	ID         string    `json:"id"`
	ExecID     string    `json:"exec_id"`
	SessionID  string    `json:"session_id,omitempty"`
	TraceID    string    `json:"trace_id,omitempty"`
	ToolName   string    `json:"tool_name"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	ToolKind   string    `json:"tool_kind,omitempty"`
	Effect     string    `json:"effect,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
	DecidedAt  time.Time `json:"decided_at,omitempty"`
	DecidedBy  string    `json:"decided_by,omitempty"`
}

type adminApprovalsResponse struct {
	Status    string                  `json:"status"`
	Approvals []adminApprovalResponse `json:"approvals"`
}

type adminApprovalDecisionRequest struct {
	Decision  string `json:"decision"`
	DecidedBy string `json:"decided_by"`
}

type adminApprovalDecisionResponse struct {
	Status   string                `json:"status"`
	Approval adminApprovalResponse `json:"approval"`
}

func adminApprovalResponseFromState(approval state.PendingApproval) adminApprovalResponse {
	return adminApprovalResponse{
		ID:         approval.ID,
		ExecID:     approval.ExecID,
		SessionID:  approval.SessionID,
		TraceID:    approval.TraceID,
		ToolName:   approval.ToolName,
		ToolCallID: approval.ToolCallID,
		ToolKind:   approval.ToolKind,
		Effect:     approval.Effect,
		Reason:     events.RedactText(approval.Reason),
		Status:     string(approval.Status),
		CreatedAt:  approval.CreatedAt,
		DecidedAt:  approval.DecidedAt,
		DecidedBy:  approval.DecidedBy,
	}
}

func (rt httpRuntime) serveAdminApprovals(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	if rt.stateStore == nil {
		writeJSON(w, http.StatusOK, adminApprovalsResponse{Status: "ok", Approvals: []adminApprovalResponse{}})
		return
	}
	approvals, err := rt.stateStore.PendingApprovals(req.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	sort.SliceStable(approvals, func(i, j int) bool {
		if !approvals[i].CreatedAt.Equal(approvals[j].CreatedAt) {
			return approvals[i].CreatedAt.Before(approvals[j].CreatedAt)
		}
		return approvals[i].ID < approvals[j].ID
	})
	response := adminApprovalsResponse{Status: "ok", Approvals: make([]adminApprovalResponse, 0, len(approvals))}
	for _, approval := range approvals {
		response.Approvals = append(response.Approvals, adminApprovalResponseFromState(approval))
	}
	writeJSON(w, http.StatusOK, response)
}

func (rt httpRuntime) serveAdminApprovalDecision(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	if rt.stateStore == nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_missing")
		return
	}
	id := strings.TrimSpace(req.PathValue("id"))
	if id == "" {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	var decision adminApprovalDecisionRequest
	if err := json.NewDecoder(req.Body).Decode(&decision); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_decision")
		return
	}
	status, ok := approvalStatusForDecision(decision.Decision)
	if !ok {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_decision")
		return
	}

	approval, ok, err := rt.stateStore.Approval(req.Context(), id)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, "state_store_error")
		return
	}
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, "not_found")
		return
	}
	if approval.Status != state.ApprovalPending {
		writeJSONStatus(w, http.StatusConflict, "already_decided")
		return
	}

	resolved, err := rt.stateStore.ResolveApproval(req.Context(), id, status, strings.TrimSpace(decision.DecidedBy))
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, "already_decided")
		return
	}
	rt.recordApprovalDecisionEvents(req.Context(), resolved)
	writeJSON(w, http.StatusOK, adminApprovalDecisionResponse{
		Status:   "ok",
		Approval: adminApprovalResponseFromState(resolved),
	})
}

func (rt httpRuntime) recordApprovalDecisionEvents(ctx context.Context, approval state.PendingApproval) {
	base := map[string]any{
		"approval_id":  approval.ID,
		"tool":         approval.ToolName,
		"tool_call_id": approval.ToolCallID,
	}
	if approval.Status == state.ApprovalApproved {
		rt.emitApprovalEvent(ctx, approval, events.EventApprovalApproved, base)
		rt.emitApprovalEvent(ctx, approval, events.EventExecutionResumed, base)
		return
	}
	rt.emitApprovalEvent(ctx, approval, events.EventApprovalDenied, base)
	// A denied gated call is a governed failure for its execution.
	if rt.stateStore != nil && approval.ExecID != "" {
		execution, ok, err := rt.stateStore.Execution(ctx, approval.ExecID)
		if err == nil && ok && execution.Status == state.ExecutionRunning {
			execution.Status = state.ExecutionFailed
			execution.CompletedAt = time.Now().UTC()
			_ = rt.stateStore.SaveExecution(ctx, execution)
		}
	}
}

func (rt httpRuntime) emitApprovalEvent(ctx context.Context, approval state.PendingApproval, kind events.EventKind, payload map[string]any) {
	if rt.stateStore == nil && rt.eventStream == nil {
		return
	}
	event := events.Event{
		Kind:      kind,
		ExecID:    approval.ExecID,
		SessionID: approval.SessionID,
		TraceID:   approval.TraceID,
		Payload:   payload,
	}
	if rt.stateStore != nil {
		if _, err := rt.stateStore.AddEvent(ctx, event); err != nil {
			return
		}
	}
	if rt.eventStream != nil {
		_, _ = rt.eventStream.Append(ctx, event)
	}
}

func approvalStatusForDecision(decision string) (state.ApprovalStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "approve", "approved", "allow":
		return state.ApprovalApproved, true
	case "deny", "denied", "reject", "rejected":
		return state.ApprovalDenied, true
	default:
		return "", false
	}
}

// writeSuspendedResponse answers a gated, suspended execution with a 202
// Accepted carrying the approval and execution identifiers so the caller can
// poll the admin approval endpoints.
func writeSuspendedResponse(w http.ResponseWriter, suspended *tools.SuspendedError) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":      "pending_approval",
		"approval_id": suspended.ApprovalID,
		"exec_id":     suspended.ExecID,
		"tool":        suspended.ToolName,
	})
}
