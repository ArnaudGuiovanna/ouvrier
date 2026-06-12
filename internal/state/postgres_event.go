package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

func (s *PostgresStore) AddEvent(ctx context.Context, event events.Event) (events.Event, error) {
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return events.Event{}, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Event IDs are serialized through the single-row counter so both the
	// auto-assign and explicit-ID paths stay collision-free across replicas.
	if event.ID == 0 {
		var id uint64
		if err := tx.QueryRowContext(ctx,
			`UPDATE ouvrier_event_counter SET last_id = last_id + 1 RETURNING last_id`).Scan(&id); err != nil {
			return events.Event{}, err
		}
		event.ID = id
	} else {
		var id uint64
		err := tx.QueryRowContext(ctx,
			`UPDATE ouvrier_event_counter SET last_id = $1 WHERE last_id < $1 RETURNING last_id`,
			event.ID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return events.Event{}, errors.New("event ID must be greater than existing event IDs")
		}
		if err != nil {
			return events.Event{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO ouvrier_events (
		id, at, kind, exec_id, session_id, trace_id, payload
	) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		event.ID,
		postgresTime(event.At),
		string(event.Kind),
		event.ExecID,
		event.SessionID,
		event.TraceID,
		string(payload),
	); err != nil {
		return events.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return events.Event{}, err
	}
	return events.SanitizeEvent(event), nil
}

func (s *PostgresStore) Events(ctx context.Context, execID string) ([]events.Event, error) {
	return s.eventsSince(ctx, execID, 0)
}

func (s *PostgresStore) EventsSince(ctx context.Context, execID string, afterID uint64) ([]events.Event, error) {
	return s.eventsSince(ctx, execID, afterID)
}

func (s *PostgresStore) eventsSince(ctx context.Context, execID string, afterID uint64) ([]events.Event, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	query := `SELECT id, at, kind, exec_id, session_id, trace_id, payload FROM ouvrier_events`
	args := []any{}
	clauses := []string{}
	if execID != "" {
		args = append(args, execID)
		clauses = append(clauses, fmt.Sprintf("exec_id = $%d", len(args)))
	}
	if afterID > 0 {
		args = append(args, int64(afterID))
		clauses = append(clauses, fmt.Sprintf("id > $%d", len(args)))
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recorded := []events.Event{}
	for rows.Next() {
		event, err := scanPostgresEvent(rows)
		if err != nil {
			return nil, err
		}
		recorded = append(recorded, event)
	}
	return recorded, rows.Err()
}

func scanPostgresEvent(scanner sqlRowScanner) (events.Event, error) {
	var event events.Event
	var at time.Time
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
	event.At = postgresTime(at)
	event.Kind = events.EventKind(kind)
	if payload.Valid && payload.String != "null" {
		if err := json.Unmarshal([]byte(payload.String), &event.Payload); err != nil {
			return events.Event{}, err
		}
	}
	return events.SanitizeEvent(event), nil
}
