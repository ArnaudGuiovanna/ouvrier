package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *SQLiteStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return "", false, err
	}
	key = strings.TrimSpace(key)
	execID = strings.TrimSpace(execID)
	if key == "" {
		return "", false, errors.New("idempotency key is required")
	}
	if execID == "" {
		return "", false, errors.New("execution ID is required")
	}

	now := formatSQLiteTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `INSERT INTO ouvrier_idempotency_keys (
		key, exec_id, created_at, outcome, updated_at
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET
		exec_id = excluded.exec_id,
		created_at = excluded.created_at,
		outcome = excluded.outcome,
		updated_at = excluded.updated_at
	WHERE ouvrier_idempotency_keys.outcome = ?`,
		key, execID, now, string(IdempotencyPending), now, string(IdempotencyFailed))
	if err != nil {
		return "", false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if affected == 1 {
		return "", true, nil
	}

	var existing string
	if err := s.db.QueryRowContext(ctx, "SELECT exec_id FROM ouvrier_idempotency_keys WHERE key = ?", key).Scan(&existing); err != nil {
		return "", false, err
	}
	return existing, false, nil
}

func (s *SQLiteStore) Idempotency(ctx context.Context, key string) (IdempotencyRecord, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return IdempotencyRecord{}, false, errors.New("idempotency key is required")
	}
	var record IdempotencyRecord
	var outcome, createdAt, updatedAt string
	err = s.db.QueryRowContext(ctx, `SELECT key, exec_id, outcome, created_at, updated_at
		FROM ouvrier_idempotency_keys WHERE key = ?`, key).Scan(
		&record.Key, &record.ExecID, &outcome, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	record.Outcome = IdempotencyOutcome(outcome)
	record.CreatedAt, err = parseSQLiteTime(createdAt)
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("parse idempotency created_at: %w", err)
	}
	record.UpdatedAt, err = parseSQLiteTime(updatedAt)
	if err != nil {
		return IdempotencyRecord{}, false, fmt.Errorf("parse idempotency updated_at: %w", err)
	}
	return record, true, nil
}

func (s *SQLiteStore) ResolveIdempotency(ctx context.Context, key, execID string, outcome IdempotencyOutcome) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	execID = strings.TrimSpace(execID)
	if err := validateIdempotencyResolution(key, execID, outcome); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ouvrier_idempotency_keys
		SET outcome = ?, updated_at = ?
		WHERE key = ? AND exec_id = ? AND outcome = ?`,
		string(outcome), formatSQLiteTime(time.Now().UTC()), key, execID, string(IdempotencyPending))
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 1 {
		return nil
	}
	record, ok, err := s.Idempotency(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("idempotency reservation not found")
	}
	if record.ExecID != execID {
		return errors.New("idempotency reservation owner mismatch")
	}
	if record.Outcome == outcome {
		return nil
	}
	return errors.New("idempotency reservation already resolved")
}

func (s *SQLiteStore) ResolveIdempotencyByExecution(ctx context.Context, execID, keyPrefix string, outcome IdempotencyOutcome) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	execID = strings.TrimSpace(execID)
	if execID == "" {
		return errors.New("execution ID is required")
	}
	if err := validateResolvedIdempotencyOutcome(outcome); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE ouvrier_idempotency_keys
		SET outcome = ?, updated_at = ?
		WHERE exec_id = ? AND outcome = ? AND substr(key, 1, ?) = ?`,
		string(outcome), formatSQLiteTime(time.Now().UTC()), execID, string(IdempotencyPending), len(keyPrefix), keyPrefix)
	return err
}
