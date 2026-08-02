package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const reserveIdempotencyQuery = `INSERT INTO ouvrier_idempotency_keys (
	key, exec_id, created_at, outcome, updated_at
) VALUES ($1, $2, $3, $4, $3)
ON CONFLICT (key) DO UPDATE SET
	exec_id = EXCLUDED.exec_id,
	created_at = EXCLUDED.created_at,
	outcome = EXCLUDED.outcome,
	updated_at = EXCLUDED.updated_at
WHERE ouvrier_idempotency_keys.outcome = $5
RETURNING exec_id`

func (s *PostgresStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
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

	var owner string
	err = s.db.QueryRowContext(ctx, reserveIdempotencyQuery,
		key, execID, postgresTime(time.Now().UTC()), string(IdempotencyPending), string(IdempotencyFailed),
	).Scan(&owner)
	if err == nil {
		return "", true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT exec_id FROM ouvrier_idempotency_keys WHERE key = $1`, key).Scan(&owner); err != nil {
		return "", false, fmt.Errorf("read existing idempotency reservation: %w", err)
	}
	return owner, false, nil
}

func (s *PostgresStore) Idempotency(ctx context.Context, key string) (IdempotencyRecord, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return IdempotencyRecord{}, false, errors.New("idempotency key is required")
	}
	var record IdempotencyRecord
	var outcome string
	err = s.db.QueryRowContext(ctx, `SELECT key, exec_id, outcome, created_at, updated_at
		FROM ouvrier_idempotency_keys WHERE key = $1`, key).Scan(
		&record.Key, &record.ExecID, &outcome, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	record.Outcome = IdempotencyOutcome(outcome)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, true, nil
}

func (s *PostgresStore) ResolveIdempotency(ctx context.Context, key, execID string, outcome IdempotencyOutcome) error {
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
		SET outcome = $1, updated_at = $2
		WHERE key = $3 AND exec_id = $4 AND outcome = $5`,
		string(outcome), postgresTime(time.Now().UTC()), key, execID, string(IdempotencyPending))
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

func (s *PostgresStore) ResolveIdempotencyByExecution(ctx context.Context, execID, keyPrefix string, outcome IdempotencyOutcome) error {
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
		SET outcome = $1, updated_at = $2
		WHERE exec_id = $3 AND outcome = $4 AND left(key, length($5)) = $5`,
		string(outcome), postgresTime(time.Now().UTC()), execID, string(IdempotencyPending), keyPrefix)
	return err
}
