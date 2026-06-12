package state

import (
	"context"
	"database/sql"
	"errors"
	"time"

	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

func (s *PostgresStore) SaveSession(ctx context.Context, session runtimecore.Session) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	if session.SessionID == "" {
		return errors.New("session ID is required")
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_sessions (
		session_id, exec_id, parent_session_id, trace_id, model, started_at,
		max_iterations, max_tokens, max_cost_usd, max_wallclock_ns
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	ON CONFLICT (session_id) DO UPDATE SET
		exec_id = excluded.exec_id,
		parent_session_id = excluded.parent_session_id,
		trace_id = excluded.trace_id,
		model = excluded.model,
		started_at = excluded.started_at,
		max_iterations = excluded.max_iterations,
		max_tokens = excluded.max_tokens,
		max_cost_usd = excluded.max_cost_usd,
		max_wallclock_ns = excluded.max_wallclock_ns`,
		session.SessionID,
		session.ExecID,
		session.ParentSessionID,
		session.TraceID,
		session.Model,
		postgresTime(session.StartedAt),
		session.Budget.MaxIterations,
		session.Budget.MaxTokens,
		session.Budget.MaxCostUSD,
		int64(session.Budget.MaxWallClock),
	)
	return err
}

func (s *PostgresStore) Session(ctx context.Context, sessionID string) (runtimecore.Session, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return runtimecore.Session{}, false, err
	}

	row := s.db.QueryRowContext(ctx, `SELECT session_id, exec_id, parent_session_id,
		trace_id, model, started_at, max_iterations, max_tokens, max_cost_usd, max_wallclock_ns
		FROM ouvrier_sessions WHERE session_id = $1`, sessionID)
	session, err := scanPostgresSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return runtimecore.Session{}, false, nil
	}
	if err != nil {
		return runtimecore.Session{}, false, err
	}
	return session, true, nil
}

func (s *PostgresStore) Sessions(ctx context.Context) ([]runtimecore.Session, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT session_id, exec_id, parent_session_id,
		trace_id, model, started_at, max_iterations, max_tokens, max_cost_usd, max_wallclock_ns
		FROM ouvrier_sessions ORDER BY started_at, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := []runtimecore.Session{}
	for rows.Next() {
		session, err := scanPostgresSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func scanPostgresSession(scanner sqlRowScanner) (runtimecore.Session, error) {
	var session runtimecore.Session
	var startedAt time.Time
	var maxWallClockNS int64
	err := scanner.Scan(
		&session.SessionID,
		&session.ExecID,
		&session.ParentSessionID,
		&session.TraceID,
		&session.Model,
		&startedAt,
		&session.Budget.MaxIterations,
		&session.Budget.MaxTokens,
		&session.Budget.MaxCostUSD,
		&maxWallClockNS,
	)
	if err != nil {
		return runtimecore.Session{}, err
	}
	session.StartedAt = postgresTime(startedAt)
	session.Budget.MaxWallClock = time.Duration(maxWallClockNS)
	return session, nil
}
