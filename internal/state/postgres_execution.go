package state

import (
	"context"
	"database/sql"
	"errors"
)

func (s *PostgresStore) SaveExecution(ctx context.Context, execution Execution) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	if execution.ExecID == "" {
		return errors.New("execution ID is required")
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_executions (
		exec_id, trace_id, status, started_at, completed_at
	) VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (exec_id) DO UPDATE SET
		trace_id = excluded.trace_id,
		status = excluded.status,
		started_at = excluded.started_at,
		completed_at = excluded.completed_at`,
		execution.ExecID,
		execution.TraceID,
		string(execution.Status),
		postgresTime(execution.StartedAt),
		nullablePostgresTime(execution.CompletedAt),
	)
	return err
}

func (s *PostgresStore) Execution(ctx context.Context, execID string) (Execution, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return Execution{}, false, err
	}

	row := s.db.QueryRowContext(ctx, `SELECT exec_id, trace_id, status, started_at, completed_at
		FROM ouvrier_executions WHERE exec_id = $1`, execID)
	execution, err := scanPostgresExecution(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Execution{}, false, nil
	}
	if err != nil {
		return Execution{}, false, err
	}
	return execution, true, nil
}

func (s *PostgresStore) Executions(ctx context.Context) ([]Execution, error) {
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
		execution, err := scanPostgresExecution(rows)
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

func scanPostgresExecution(scanner sqlRowScanner) (Execution, error) {
	var execution Execution
	var status string
	var startedAt sql.NullTime
	var completedAt sql.NullTime
	if err := scanner.Scan(&execution.ExecID, &execution.TraceID, &status, &startedAt, &completedAt); err != nil {
		return Execution{}, err
	}
	execution.Status = ExecutionStatus(status)
	execution.StartedAt = nullablePostgresTimeValue(startedAt)
	execution.CompletedAt = nullablePostgresTimeValue(completedAt)
	return execution, nil
}
