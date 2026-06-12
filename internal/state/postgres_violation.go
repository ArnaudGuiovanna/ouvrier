package state

import (
	"context"
	"fmt"
	"time"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

func (s *PostgresStore) AddSchemaViolation(ctx context.Context, violation SchemaViolation) (SchemaViolation, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return SchemaViolation{}, err
	}
	if violation.At.IsZero() {
		violation.At = time.Now().UTC()
	}
	violation.Error = events.RedactText(violation.Error)

	var id uint64
	err = s.db.QueryRowContext(ctx, `INSERT INTO ouvrier_schema_violations (
		at, exec_id, session_id, schema_name, error
	) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		postgresTime(violation.At),
		violation.ExecID,
		violation.SessionID,
		violation.SchemaName,
		violation.Error,
	).Scan(&id)
	if err != nil {
		return SchemaViolation{}, err
	}
	violation.ID = id
	return violation, nil
}

func (s *PostgresStore) SchemaViolations(ctx context.Context, execID string) ([]SchemaViolation, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	query := `SELECT id, at, exec_id, session_id, schema_name, error
		FROM ouvrier_schema_violations`
	args := []any{}
	if execID != "" {
		args = append(args, execID)
		query += fmt.Sprintf(" WHERE exec_id = $%d", len(args))
	}
	query += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	violations := []SchemaViolation{}
	for rows.Next() {
		violation, err := scanPostgresViolation(rows)
		if err != nil {
			return nil, err
		}
		violations = append(violations, violation)
	}
	return violations, rows.Err()
}

func scanPostgresViolation(scanner sqlRowScanner) (SchemaViolation, error) {
	var violation SchemaViolation
	var at time.Time
	if err := scanner.Scan(
		&violation.ID,
		&at,
		&violation.ExecID,
		&violation.SessionID,
		&violation.SchemaName,
		&violation.Error,
	); err != nil {
		return SchemaViolation{}, err
	}
	violation.At = postgresTime(at)
	violation.Error = events.RedactText(violation.Error)
	return violation, nil
}
