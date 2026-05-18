package state

import (
	"context"
	"errors"
	"time"
)

func (s *SQLiteStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return "", false, err
	}
	if key == "" {
		return "", false, errors.New("idempotency key is required")
	}
	if execID == "" {
		return "", false, errors.New("execution ID is required")
	}

	result, err := s.db.ExecContext(ctx, `INSERT INTO ouvrier_idempotency_keys (
		key, exec_id, created_at
	) VALUES (?, ?, ?)
	ON CONFLICT(key) DO NOTHING`, key, execID, formatSQLiteTime(time.Now().UTC()))
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
	err = s.db.QueryRowContext(ctx, "SELECT exec_id FROM ouvrier_idempotency_keys WHERE key = ?", key).Scan(&existing)
	if err != nil {
		return "", false, err
	}
	return existing, false, nil
}
