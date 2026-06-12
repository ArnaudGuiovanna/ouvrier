package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Lease is one fenced TTL lease on shared state, used for cron leader
// election and durable-run recovery claims. The fence is a per-name token
// that increases on every takeover, so a holder presenting a stale fence can
// never extend or release a lease it has lost.
type Lease struct {
	Name       string
	Holder     string
	Fence      uint64
	AcquiredAt time.Time
	RenewedAt  time.Time
	ExpiresAt  time.Time
}

// LeaseStore is the optional fenced-lease capability discovered via
// store.(state.LeaseStore); the core Store interface is untouched. All
// implementations share these semantics:
//
//   - AcquireLease returns (lease, true, nil) when the caller acquired the
//     lease: either no row had ever existed for the name (fence starts at 1)
//     or the existing row had expired or been released (takeover; fence =
//     previous fence + 1). When the lease is held and not expired — including
//     by the caller itself — it returns the current lease and false; there is
//     no holder self-reacquire path, fence-checked RenewLease is the only
//     extension path.
//   - RenewLease succeeds only when holder AND fence match the current row
//     (i.e. the lease has not been taken over); it extends ExpiresAt by ttl
//     from now and updates RenewedAt, fence unchanged. An expired lease that
//     no contender has taken over yet can still be renewed: the fence, not
//     wall-clock expiry, is the safety boundary. On failure it returns the
//     current lease (zero Lease when none) and false.
//   - ReleaseLease tombstones the row when holder and fence match: a single
//     update sets ExpiresAt to a moment in the past (by database time) and
//     keeps the row, holder and fence included, so the next AcquireLease is a
//     takeover at the previous fence + 1. It is idempotent — releasing an
//     absent, non-held, or wrong-fence lease is a nil no-op. Release never
//     deletes the row: fences are therefore strictly monotonic per name for
//     the lifetime of the row and are usable as external fencing tokens, and
//     a holder that releases (or loses) the lease and later re-acquires can
//     never be re-issued a fence it already held — so a zombie session from
//     its previous incarnation can never renew with its old fence. Future
//     consumers may prune long-dead lease rows out-of-band. Because the
//     tombstone keeps holder and fence, the releasing holder itself could
//     still renew its released lease back to life with its current fence;
//     release-then-renew within one incarnation is a caller bug, not a
//     split-brain (a fence never identifies two sessions).
//   - Leases returns every stored row sorted by name, expired rows included;
//     callers compare ExpiresAt themselves for observability.
//
// Expiry is always evaluated against database time (now() in Postgres,
// strftime in SQLite; the memory backend uses time.Now()), so worker replicas
// with skewed clocks cannot disagree about expiry, and fences neutralize the
// effects of modest residual skew.
type LeaseStore interface {
	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (Lease, bool, error)
	RenewLease(ctx context.Context, name, holder string, fence uint64, ttl time.Duration) (Lease, bool, error)
	ReleaseLease(ctx context.Context, name, holder string, fence uint64) error
	Leases(ctx context.Context) ([]Lease, error)
}

// leaseAcquireAttempts bounds the retry loop in the SQL backends for the rare
// window where an acquire loses the upsert race but the winning row vanishes
// before the loser can read it back. Release tombstones rows rather than
// deleting them, so today only out-of-band pruning can open that window.
const leaseAcquireAttempts = 3

// normalizeLeaseKey validates and trims the (name, holder) pair shared by
// every LeaseStore method.
func normalizeLeaseKey(name, holder string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", errors.New("lease name is required")
	}
	holder = strings.TrimSpace(holder)
	if holder == "" {
		return "", "", errors.New("lease holder is required")
	}
	return name, holder, nil
}

func validateLeaseTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("lease ttl must be positive, got %s", ttl)
	}
	return nil
}
