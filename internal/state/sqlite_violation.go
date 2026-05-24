package state

import (
	"context"
	"time"

	"ouvrier/internal/events"
)

func (s *SQLiteStore) AddSchemaViolation(ctx context.Context, violation SchemaViolation) (SchemaViolation, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return SchemaViolation{}, err
	}
	if violation.At.IsZero() {
		violation.At = time.Now().UTC()
	}
	violation.Error = events.RedactText(violation.Error)

	result, err := s.db.ExecContext(ctx, `INSERT INTO ouvrier_schema_violations (
		at, exec_id, session_id, schema_name, error
	) VALUES (?, ?, ?, ?, ?)`,
		formatSQLiteTime(violation.At),
		violation.ExecID,
		violation.SessionID,
		violation.SchemaName,
		violation.Error,
	)
	if err != nil {
		return SchemaViolation{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return SchemaViolation{}, err
	}
	violation.ID = uint64(id)
	return violation, nil
}

func (s *SQLiteStore) SchemaViolations(ctx context.Context, execID string) ([]SchemaViolation, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	query := `SELECT id, at, exec_id, session_id, schema_name, error
		FROM ouvrier_schema_violations`
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

	violations := []SchemaViolation{}
	for rows.Next() {
		violation, err := scanSQLiteViolation(rows)
		if err != nil {
			return nil, err
		}
		violations = append(violations, violation)
	}
	return violations, rows.Err()
}

type sqliteViolationScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteViolation(scanner sqliteViolationScanner) (SchemaViolation, error) {
	var violation SchemaViolation
	var at string
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
	parsed, err := parseSQLiteTime(at)
	if err != nil {
		return SchemaViolation{}, err
	}
	violation.At = parsed
	return violation, nil
}
