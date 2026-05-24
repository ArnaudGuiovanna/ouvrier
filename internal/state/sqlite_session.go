package state

import (
	"context"
	"database/sql"
	"errors"
	"time"

	runtimecore "github.com/ArnaudGuiovanna/ouvrier/internal/runtime"
)

func (s *SQLiteStore) SaveSession(ctx context.Context, session runtimecore.Session) error {
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
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(session_id) DO UPDATE SET
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
		formatSQLiteTime(session.StartedAt),
		session.Budget.MaxIterations,
		session.Budget.MaxTokens,
		session.Budget.MaxCostUSD,
		int64(session.Budget.MaxWallClock),
	)
	return err
}

func (s *SQLiteStore) Session(ctx context.Context, sessionID string) (runtimecore.Session, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return runtimecore.Session{}, false, err
	}

	session, err := s.querySession(ctx, `SELECT session_id, exec_id, parent_session_id,
		trace_id, model, started_at, max_iterations, max_tokens, max_cost_usd, max_wallclock_ns
		FROM ouvrier_sessions WHERE session_id = ?`, sessionID)
	if missingSQLiteRow(err) {
		return runtimecore.Session{}, false, nil
	}
	if err != nil {
		return runtimecore.Session{}, false, err
	}
	return session, true, nil
}

func (s *SQLiteStore) Sessions(ctx context.Context) ([]runtimecore.Session, error) {
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
		session, err := scanSQLiteSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *SQLiteStore) querySession(ctx context.Context, query string, args ...any) (runtimecore.Session, error) {
	return scanSQLiteSession(s.db.QueryRowContext(ctx, query, args...))
}

type sqliteSessionScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteSession(scanner sqliteSessionScanner) (runtimecore.Session, error) {
	var session runtimecore.Session
	var startedAt string
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
	session.StartedAt, err = parseSQLiteTime(startedAt)
	if err != nil {
		return runtimecore.Session{}, err
	}
	session.Budget.MaxWallClock = time.Duration(maxWallClockNS)
	return session, nil
}

var _ sqliteSessionScanner = (*sql.Row)(nil)
var _ sqliteSessionScanner = (*sql.Rows)(nil)
