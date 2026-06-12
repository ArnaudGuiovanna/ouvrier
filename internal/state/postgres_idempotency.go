package state

import (
	"context"
	"errors"
	"time"
)

func (s *PostgresStore) ReserveIdempotency(ctx context.Context, key, execID string) (string, bool, error) {
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

	// Single-statement reservation that is atomic across replicas: a no-op
	// conflict update lets RETURNING expose the winning row, and xmax = 0
	// distinguishes a fresh insert (we won) from an updated existing row
	// (someone else won).
	var owner string
	var inserted bool
	err = s.db.QueryRowContext(ctx, `INSERT INTO ouvrier_idempotency_keys (
		key, exec_id, created_at
	) VALUES ($1, $2, $3)
	ON CONFLICT (key) DO UPDATE SET exec_id = ouvrier_idempotency_keys.exec_id
	RETURNING exec_id, (xmax = 0)`,
		key, execID, postgresTime(time.Now().UTC()),
	).Scan(&owner, &inserted)
	if err != nil {
		return "", false, err
	}
	if inserted {
		return "", true, nil
	}
	return owner, false, nil
}
