package state

import (
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

// normalizeApproval validates and redaction-cleans an approval write shared by
// every Store backend. It trims identifiers, requires the minimal context
// needed to resume an execution, and scrubs the human-readable reason with the
// same credential redaction used for persisted events so no secret reaches
// durable storage.
func normalizeApproval(approval PendingApproval) (PendingApproval, error) {
	approval.ID = strings.TrimSpace(approval.ID)
	if approval.ID == "" {
		return PendingApproval{}, fmt.Errorf("approval id is required")
	}
	approval.ExecID = strings.TrimSpace(approval.ExecID)
	if approval.ExecID == "" {
		return PendingApproval{}, fmt.Errorf("approval execution id is required")
	}
	approval.ToolName = strings.TrimSpace(approval.ToolName)
	if approval.ToolName == "" {
		return PendingApproval{}, fmt.Errorf("approval tool name is required")
	}
	if approval.Status == "" {
		approval.Status = ApprovalPending
	}
	approval.SessionID = strings.TrimSpace(approval.SessionID)
	approval.TraceID = strings.TrimSpace(approval.TraceID)
	approval.ToolCallID = strings.TrimSpace(approval.ToolCallID)
	approval.ToolKind = strings.TrimSpace(approval.ToolKind)
	approval.Effect = strings.TrimSpace(approval.Effect)
	approval.DecidedBy = strings.TrimSpace(approval.DecidedBy)
	approval.Reason = events.RedactText(strings.TrimSpace(approval.Reason))
	return approval, nil
}

// terminalApprovalStatus reports whether status is a valid resolution decision.
func terminalApprovalStatus(status ApprovalStatus) bool {
	return status == ApprovalApproved || status == ApprovalDenied
}
