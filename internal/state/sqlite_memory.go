package state

import (
	"context"
	"strings"
	"time"
)

func (s *SQLiteStore) SaveMemory(ctx context.Context, scope, key, value string) error {
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
	) VALUES (?, ?, ?, ?)
	ON CONFLICT(scope, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		scope, key, value, formatSQLiteTime(time.Now().UTC()),
	)
	return err
}

func (s *SQLiteStore) Memory(ctx context.Context, scope, key string) (string, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return "", false, err
	}

	var value string
	err = s.db.QueryRowContext(ctx,
		"SELECT value FROM ouvrier_memory WHERE scope = ? AND key = ?",
		strings.TrimSpace(scope), strings.TrimSpace(key),
	).Scan(&value)
	if missingSQLiteRow(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *SQLiteStore) ListMemory(ctx context.Context, scope string) ([]MemoryRecord, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		"SELECT scope, key, value, updated_at FROM ouvrier_memory WHERE scope = ? ORDER BY key",
		strings.TrimSpace(scope),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []MemoryRecord{}
	for rows.Next() {
		var record MemoryRecord
		var updatedAt string
		if err := rows.Scan(&record.Scope, &record.Key, &record.Value, &updatedAt); err != nil {
			return nil, err
		}
		parsed, err := parseSQLiteTime(updatedAt)
		if err != nil {
			return nil, err
		}
		record.UpdatedAt = parsed
		records = append(records, record)
	}
	return records, rows.Err()
}
