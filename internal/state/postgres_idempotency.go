package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// reserveIdempotencyQuery is a single-statement reservation that is atomic
// across replicas. The CTE attempts the insert; if it succeeds, the branch
// with inserted = TRUE exists and the ORDER BY makes it win the LIMIT 1
// (UNION ALL gives no row-order guarantee on its own). If the key already
// exists, the insert returns nothing and the second branch reports the
// committed owner with inserted = FALSE. This avoids relying on the
// undocumented (xmax = 0) system-column behavior.
const reserveIdempotencyQuery = `WITH ins AS (
	INSERT INTO ouvrier_idempotency_keys (key, exec_id, created_at)
	VALUES ($1, $2, $3)
	ON CONFLICT (key) DO NOTHING
	RETURNING exec_id
)
SELECT exec_id, TRUE AS inserted FROM ins
UNION ALL
SELECT exec_id, FALSE FROM ouvrier_idempotency_keys WHERE key = $1
ORDER BY inserted DESC
LIMIT 1`

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

	// With ON CONFLICT DO NOTHING there is a rare race: a concurrent,
	// not-yet-committed insert blocks our insert (first branch empty) and is
	// then skipped once it commits, while the fallback SELECT still uses the
	// statement snapshot taken before that commit (second branch empty too).
	// A single retry re-runs the statement with a fresh snapshot that sees
	// the winner, so two attempts always suffice.
	for attempt := 0; attempt < 2; attempt++ {
		var owner string
		var inserted bool
		err = s.db.QueryRowContext(ctx, reserveIdempotencyQuery,
			key, execID, postgresTime(time.Now().UTC()),
		).Scan(&owner, &inserted)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if inserted {
			return "", true, nil
		}
		return owner, false, nil
	}
	return "", false, fmt.Errorf("reserve idempotency key %q: no row after retry", key)
}
