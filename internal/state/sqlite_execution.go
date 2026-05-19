package state

import (
	"context"
	"database/sql"
	"errors"
)

func (s *SQLiteStore) SaveExecution(ctx context.Context, execution Execution) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	if execution.ExecID == "" {
		return errors.New("execution ID is required")
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_executions (
		exec_id, trace_id, status, started_at, completed_at
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(exec_id) DO UPDATE SET
		trace_id = excluded.trace_id,
		status = excluded.status,
		started_at = excluded.started_at,
		completed_at = excluded.completed_at`,
		execution.ExecID,
		execution.TraceID,
		string(execution.Status),
		formatSQLiteTime(execution.StartedAt),
		nullableSQLiteTime(execution.CompletedAt),
	)
	return err
}

func (s *SQLiteStore) Execution(ctx context.Context, execID string) (Execution, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return Execution{}, false, err
	}

	var execution Execution
	var status string
	var startedAt string
	var completedAt sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT exec_id, trace_id, status, started_at, completed_at
		FROM ouvrier_executions WHERE exec_id = ?`, execID).
		Scan(&execution.ExecID, &execution.TraceID, &status, &startedAt, &completedAt)
	if missingSQLiteRow(err) {
		return Execution{}, false, nil
	}
	if err != nil {
		return Execution{}, false, err
	}

	execution.Status = ExecutionStatus(status)
	execution.StartedAt, err = parseSQLiteTime(startedAt)
	if err != nil {
		return Execution{}, false, err
	}
	execution.CompletedAt, err = parseNullableSQLiteTime(completedAt)
	if err != nil {
		return Execution{}, false, err
	}
	return execution, true, nil
}

func (s *SQLiteStore) Executions(ctx context.Context) ([]Execution, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT exec_id, trace_id, status, started_at, completed_at
		FROM ouvrier_executions ORDER BY started_at ASC, exec_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var executions []Execution
	for rows.Next() {
		execution, err := scanSQLiteExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return executions, nil
}

type sqliteExecutionScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteExecution(scanner sqliteExecutionScanner) (Execution, error) {
	var execution Execution
	var status string
	var startedAt string
	var completedAt sql.NullString
	if err := scanner.Scan(&execution.ExecID, &execution.TraceID, &status, &startedAt, &completedAt); err != nil {
		return Execution{}, err
	}

	execution.Status = ExecutionStatus(status)
	var err error
	execution.StartedAt, err = parseSQLiteTime(startedAt)
	if err != nil {
		return Execution{}, err
	}
	execution.CompletedAt, err = parseNullableSQLiteTime(completedAt)
	if err != nil {
		return Execution{}, err
	}
	return execution, nil
}
