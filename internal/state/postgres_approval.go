package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *PostgresStore) SaveApproval(ctx context.Context, approval PendingApproval) error {
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
		effect, reason, args_hash, status, created_at, decided_at, decided_by
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	ON CONFLICT (id) DO UPDATE SET
		exec_id = excluded.exec_id,
		session_id = excluded.session_id,
		trace_id = excluded.trace_id,
		tool_name = excluded.tool_name,
		tool_call_id = excluded.tool_call_id,
		tool_kind = excluded.tool_kind,
		effect = excluded.effect,
		reason = excluded.reason,
		args_hash = excluded.args_hash,
		status = excluded.status,
		decided_at = excluded.decided_at,
		decided_by = excluded.decided_by`,
		approval.ID, approval.ExecID, approval.SessionID, approval.TraceID,
		approval.ToolName, approval.ToolCallID, approval.ToolKind, approval.Effect,
		approval.Reason, approval.ArgsHash, string(approval.Status), postgresTime(approval.CreatedAt),
		nullablePostgresTime(approval.DecidedAt), approval.DecidedBy,
	)
	return err
}

func (s *PostgresStore) Approval(ctx context.Context, id string) (PendingApproval, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return PendingApproval{}, false, err
	}
	row := s.db.QueryRowContext(ctx,
		postgresApprovalSelectColumns+" FROM ouvrier_approvals WHERE id = $1", strings.TrimSpace(id))
	approval, err := scanPostgresApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingApproval{}, false, nil
	}
	if err != nil {
		return PendingApproval{}, false, err
	}
	return approval, true, nil
}

func (s *PostgresStore) PendingApprovals(ctx context.Context) ([]PendingApproval, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		postgresApprovalSelectColumns+" FROM ouvrier_approvals WHERE status = $1 ORDER BY seq", string(ApprovalPending))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	approvals := []PendingApproval{}
	for rows.Next() {
		approval, err := scanPostgresApproval(rows)
		if err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

func (s *PostgresStore) ApprovalsForExecution(ctx context.Context, execID string) ([]PendingApproval, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		postgresApprovalSelectColumns+" FROM ouvrier_approvals WHERE exec_id = $1 ORDER BY seq", strings.TrimSpace(execID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	approvals := []PendingApproval{}
	for rows.Next() {
		approval, err := scanPostgresApproval(rows)
		if err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

func (s *PostgresStore) ResolveApproval(ctx context.Context, id string, status ApprovalStatus, decidedBy string) (PendingApproval, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return PendingApproval{}, err
	}
	if !terminalApprovalStatus(status) {
		return PendingApproval{}, errors.New("approval resolution must be approved or denied")
	}
	id = strings.TrimSpace(id)

	// Single-statement resolution: the status = 'pending' guard makes exactly
	// one resolver win across replicas, and RETURNING hands the winner the
	// decided row without a second round trip.
	row := s.db.QueryRowContext(ctx, `UPDATE ouvrier_approvals
		SET status = $1, decided_by = $2, decided_at = $3
		WHERE id = $4 AND status = $5
		RETURNING id, exec_id, session_id, trace_id, tool_name, tool_call_id,
			tool_kind, effect, reason, args_hash, status, created_at, decided_at, decided_by`,
		string(status), strings.TrimSpace(decidedBy), postgresTime(time.Now().UTC()), id, string(ApprovalPending))
	approval, err := scanPostgresApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		_, ok, getErr := s.Approval(ctx, id)
		if getErr != nil {
			return PendingApproval{}, getErr
		}
		if !ok {
			return PendingApproval{}, errors.New("approval not found")
		}
		return PendingApproval{}, errors.New("approval already decided")
	}
	if err != nil {
		return PendingApproval{}, err
	}
	return approval, nil
}

const postgresApprovalSelectColumns = `SELECT id, exec_id, session_id, trace_id, tool_name, tool_call_id,
	tool_kind, effect, reason, args_hash, status, created_at, decided_at, decided_by`

func scanPostgresApproval(row sqlRowScanner) (PendingApproval, error) {
	var approval PendingApproval
	var status string
	var createdAt time.Time
	var decidedAt sql.NullTime
	if err := row.Scan(
		&approval.ID, &approval.ExecID, &approval.SessionID, &approval.TraceID,
		&approval.ToolName, &approval.ToolCallID, &approval.ToolKind, &approval.Effect,
		&approval.Reason, &approval.ArgsHash, &status, &createdAt, &decidedAt, &approval.DecidedBy,
	); err != nil {
		return PendingApproval{}, err
	}
	approval.Status = ApprovalStatus(status)
	approval.CreatedAt = postgresTime(createdAt)
	approval.DecidedAt = nullablePostgresTimeValue(decidedAt)
	return approval, nil
}
