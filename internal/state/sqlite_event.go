package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"ouvrier/internal/events"
)

func (s *SQLiteStore) AddEvent(ctx context.Context, event events.Event) (events.Event, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return events.Event{}, err
	}
	event = events.SanitizeEvent(event)
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return events.Event{}, err
	}

	result, err := s.db.ExecContext(ctx, `INSERT INTO ouvrier_events (
		at, kind, exec_id, session_id, trace_id, payload
	) VALUES (?, ?, ?, ?, ?, ?)`,
		formatSQLiteTime(event.At),
		string(event.Kind),
		event.ExecID,
		event.SessionID,
		event.TraceID,
		string(payload),
	)
	if err != nil {
		return events.Event{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return events.Event{}, err
	}
	event.ID = uint64(id)
	return events.SanitizeEvent(event), nil
}

func (s *SQLiteStore) Events(ctx context.Context, execID string) ([]events.Event, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	query := `SELECT id, at, kind, exec_id, session_id, trace_id, payload FROM ouvrier_events`
	args := []any{}
	if execID != "" {
		query += " WHERE exec_id = ?"
		args = append(args, execID)
	}
	query += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recorded := []events.Event{}
	for rows.Next() {
		event, err := scanSQLiteEvent(rows)
		if err != nil {
			return nil, err
		}
		recorded = append(recorded, event)
	}
	return recorded, rows.Err()
}

type sqliteEventScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteEvent(scanner sqliteEventScanner) (events.Event, error) {
	var event events.Event
	var at string
	var kind string
	var payload sql.NullString
	if err := scanner.Scan(
		&event.ID,
		&at,
		&kind,
		&event.ExecID,
		&event.SessionID,
		&event.TraceID,
		&payload,
	); err != nil {
		return events.Event{}, err
	}
	parsed, err := parseSQLiteTime(at)
	if err != nil {
		return events.Event{}, err
	}
	event.At = parsed
	event.Kind = events.EventKind(kind)
	if payload.Valid && payload.String != "null" {
		if err := json.Unmarshal([]byte(payload.String), &event.Payload); err != nil {
			return events.Event{}, err
		}
	}
	return events.SanitizeEvent(event), nil
}
