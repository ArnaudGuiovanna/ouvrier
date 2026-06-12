package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *PostgresStore) SaveMemory(ctx context.Context, scope, key, value string) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	scope, key, value, normErr := normalizeMemory(scope, key, value)
	if normErr != nil {
		return normErr
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO ouvrier_memory (
		scope, key, value, updated_at
	) VALUES ($1, $2, $3, $4)
	ON CONFLICT (scope, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		scope, key, value, postgresTime(time.Now().UTC()),
	)
	return err
}

func (s *PostgresStore) Memory(ctx context.Context, scope, key string) (string, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return "", false, err
	}

	var value string
	err = s.db.QueryRowContext(ctx,
		"SELECT value FROM ouvrier_memory WHERE scope = $1 AND key = $2",
		strings.TrimSpace(scope), strings.TrimSpace(key),
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *PostgresStore) ListMemory(ctx context.Context, scope string) ([]MemoryRecord, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT scope, key, value, updated_at FROM ouvrier_memory WHERE scope = $1 ORDER BY key",
		strings.TrimSpace(scope),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []MemoryRecord{}
	for rows.Next() {
		var record MemoryRecord
		var updatedAt time.Time
		if err := rows.Scan(&record.Scope, &record.Key, &record.Value, &updatedAt); err != nil {
			return nil, err
		}
		record.UpdatedAt = postgresTime(updatedAt)
		records = append(records, record)
	}
	return records, rows.Err()
}
