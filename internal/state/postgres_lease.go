package state

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// All lease expiry comparisons run against the database clock (now()), so
// worker replicas with skewed process clocks cannot disagree about expiry.

const postgresLeaseColumns = `name, holder, fence, acquired_at, renewed_at, expires_at`

func (s *PostgresStore) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (Lease, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return Lease{}, false, err
	}
	name, holder, err = normalizeLeaseKey(name, holder)
	if err != nil {
		return Lease{}, false, err
	}
	if err := validateLeaseTTL(ttl); err != nil {
		return Lease{}, false, err
	}

	// One atomic upsert: insert a fresh lease at fence 1, or take over an
	// expired row at the previous fence + 1. There is intentionally no holder
	// self-reacquire clause — fence-checked RenewLease is the only extension
	// path. The retry loop covers the rare window where the upsert loses but
	// the winning row is released before the loser reads it back; after the
	// attempts are exhausted the caller simply observes "not acquired" and
	// polls again.
	acquire := `INSERT INTO ouvrier_leases (` + postgresLeaseColumns + `)
		VALUES ($1, $2, 1, now(), now(), now() + make_interval(secs => $3))
		ON CONFLICT (name) DO UPDATE SET
			holder = excluded.holder,
			fence = ouvrier_leases.fence + 1,
			acquired_at = excluded.acquired_at,
			renewed_at = excluded.renewed_at,
			expires_at = excluded.expires_at
		WHERE ouvrier_leases.expires_at < now()
		RETURNING ` + postgresLeaseColumns
	for attempt := 0; attempt < leaseAcquireAttempts; attempt++ {
		row := s.db.QueryRowContext(ctx, acquire, name, holder, ttl.Seconds())
		lease, err := scanPostgresLease(row)
		if err == nil {
			return lease, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Lease{}, false, err
		}
		current, ok, err := s.leaseByName(ctx, name)
		if err != nil {
			return Lease{}, false, err
		}
		if ok {
			return current, false, nil
		}
	}
	return Lease{}, false, nil
}

func (s *PostgresStore) RenewLease(ctx context.Context, name, holder string, fence uint64, ttl time.Duration) (Lease, bool, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return Lease{}, false, err
	}
	name, holder, err = normalizeLeaseKey(name, holder)
	if err != nil {
		return Lease{}, false, err
	}
	if err := validateLeaseTTL(ttl); err != nil {
		return Lease{}, false, err
	}

	row := s.db.QueryRowContext(ctx, `UPDATE ouvrier_leases SET
			renewed_at = now(),
			expires_at = now() + make_interval(secs => $1)
		WHERE name = $2 AND holder = $3 AND fence = $4
		RETURNING `+postgresLeaseColumns,
		ttl.Seconds(), name, holder, int64(fence))
	lease, err := scanPostgresLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		current, _, err := s.leaseByName(ctx, name)
		if err != nil {
			return Lease{}, false, err
		}
		return current, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	return lease, true, nil
}

func (s *PostgresStore) ReleaseLease(ctx context.Context, name, holder string, fence uint64) error {
	ctx, err := activeContext(ctx)
	if err != nil {
		return err
	}
	name, holder, err = normalizeLeaseKey(name, holder)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM ouvrier_leases WHERE name = $1 AND holder = $2 AND fence = $3`,
		name, holder, int64(fence))
	return err
}

func (s *PostgresStore) Leases(ctx context.Context) ([]Lease, error) {
	ctx, err := activeContext(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+postgresLeaseColumns+` FROM ouvrier_leases ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	leases := []Lease{}
	for rows.Next() {
		lease, err := scanPostgresLease(rows)
		if err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, rows.Err()
}

func (s *PostgresStore) leaseByName(ctx context.Context, name string) (Lease, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+postgresLeaseColumns+` FROM ouvrier_leases WHERE name = $1`, name)
	lease, err := scanPostgresLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	return lease, true, nil
}

func scanPostgresLease(row sqlRowScanner) (Lease, error) {
	var lease Lease
	var fence int64
	var acquiredAt, renewedAt, expiresAt time.Time
	if err := row.Scan(&lease.Name, &lease.Holder, &fence, &acquiredAt, &renewedAt, &expiresAt); err != nil {
		return Lease{}, err
	}
	lease.Fence = uint64(fence)
	lease.AcquiredAt = postgresTime(acquiredAt)
	lease.RenewedAt = postgresTime(renewedAt)
	lease.ExpiresAt = postgresTime(expiresAt)
	return lease, nil
}
