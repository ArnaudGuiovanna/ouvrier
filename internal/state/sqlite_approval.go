package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *SQLiteStore) SaveApproval(ctx context.Context, approval PendingApproval) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	approval, normErr := normalizeApproval(approval)
	if normErr != nil {
		return normErr
	}
	if approval.CreatedAt.IsZero() {
		approval.CreatedAt = time.Now().UTC()
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_approvals (
		id, exec_id, session_id, trace_id, tool_name, tool_call_id, tool_kind,
		effect, reason, status, created_at, decided_at, decided_by
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		exec_id = excluded.exec_id,
		session_id = excluded.session_id,
		trace_id = excluded.trace_id,
		tool_name = excluded.tool_name,
		tool_call_id = excluded.tool_call_id,
		tool_kind = excluded.tool_kind,
		effect = excluded.effect,
		reason = excluded.reason,
		status = excluded.status,
		decided_at = excluded.decided_at,
		decided_by = excluded.decided_by`,
		approval.ID, approval.ExecID, approval.SessionID, approval.TraceID,
		approval.ToolName, approval.ToolCallID, approval.ToolKind, approval.Effect,
		approval.Reason, string(approval.Status), formatSQLiteTime(approval.CreatedAt),
		nullableSQLiteTime(approval.DecidedAt), approval.DecidedBy,
	)
	return err
}

func (s *SQLiteStore) Approval(ctx context.Context, id string) (PendingApproval, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return PendingApproval{}, false, err
	}
	row := s.db.QueryRowContext(ctx, approvalSelectColumns+" FROM ouvrier_approvals WHERE id = ?", strings.TrimSpace(id))
	approval, err := scanApprovalRow(row)
	if missingSQLiteRow(err) {
		return PendingApproval{}, false, nil
	}
	if err != nil {
		return PendingApproval{}, false, err
	}
	return approval, true, nil
}

func (s *SQLiteStore) PendingApprovals(ctx context.Context) ([]PendingApproval, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, approvalSelectColumns+" FROM ouvrier_approvals WHERE status = ? ORDER BY seq", string(ApprovalPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	approvals := []PendingApproval{}
	for rows.Next() {
		approval, err := scanApprovalRow(rows)
		if err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

func (s *SQLiteStore) ResolveApproval(ctx context.Context, id string, status ApprovalStatus, decidedBy string) (PendingApproval, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return PendingApproval{}, err
	}
	if !terminalApprovalStatus(status) {
		return PendingApproval{}, errors.New("approval resolution must be approved or denied")
	}
	id = strings.TrimSpace(id)

	result, err := s.db.ExecContext(ctx,
		"UPDATE ouvrier_approvals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ? AND status = ?",
		string(status), strings.TrimSpace(decidedBy), formatSQLiteTime(time.Now().UTC()), id, string(ApprovalPending),
	)
	if err != nil {
		return PendingApproval{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return PendingApproval{}, err
	}
	if affected == 0 {
		approval, ok, getErr := s.Approval(ctx, id)
		if getErr != nil {
			return PendingApproval{}, getErr
		}
		if !ok {
			return PendingApproval{}, errors.New("approval not found")
		}
		_ = approval
		return PendingApproval{}, errors.New("approval already decided")
	}
	approval, _, err := s.Approval(ctx, id)
	if err != nil {
		return PendingApproval{}, err
	}
	return approval, nil
}

const approvalSelectColumns = `SELECT id, exec_id, session_id, trace_id, tool_name, tool_call_id,
	tool_kind, effect, reason, status, created_at, decided_at, decided_by`

type sqlRowScanner interface {
	Scan(dest ...any) error
}

func scanApprovalRow(row sqlRowScanner) (PendingApproval, error) {
	var approval PendingApproval
	var status, createdAt string
	var decidedAt sql.NullString
	if err := row.Scan(
		&approval.ID, &approval.ExecID, &approval.SessionID, &approval.TraceID,
		&approval.ToolName, &approval.ToolCallID, &approval.ToolKind, &approval.Effect,
		&approval.Reason, &status, &createdAt, &decidedAt, &approval.DecidedBy,
	); err != nil {
		return PendingApproval{}, err
	}
	approval.Status = ApprovalStatus(status)
	created, err := parseSQLiteTime(createdAt)
	if err != nil {
		return PendingApproval{}, err
	}
	approval.CreatedAt = created
	decided, err := parseNullableSQLiteTime(decidedAt)
	if err != nil {
		return PendingApproval{}, err
	}
	approval.DecidedAt = decided
	return approval, nil
}
