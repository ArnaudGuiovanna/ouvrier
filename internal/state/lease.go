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
//     lease: either no row existed (fence starts at 1) or the existing row had
//     expired (takeover; fence = previous fence + 1, so fences are strictly
//     monotonic across takeovers). When the lease is held and not expired —
//     including by the caller itself — it returns the current lease and false;
//     there is no holder self-reacquire path, fence-checked RenewLease is the
//     only extension path.
//   - RenewLease succeeds only when holder AND fence match the current row
//     (i.e. the lease has not been taken over or released); it extends
//     ExpiresAt by ttl from now and updates RenewedAt, fence unchanged. An
//     expired lease that no contender has taken over yet can still be renewed:
//     the fence, not wall-clock expiry, is the safety boundary. On failure it
//     returns the current lease (zero Lease when none) and false.
//   - ReleaseLease deletes the row only when holder and fence match; it is
//     idempotent — releasing an absent or non-held lease is not an error.
//     Because release deletes the row, a subsequent acquire restarts at
//     fence 1.
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
// window where an acquire loses the upsert race but the winning row is
// released before the loser can read it back.
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
