package state

import (
	"context"
	"sort"
	"time"
)

// The memory backend evaluates lease expiry with time.Now(): it lives inside a
// single process, so "database time" and process time are the same clock.

func (s *MemoryStore) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (Lease, bool, error) {
	if err := checkContext(ctx); err != nil {
		return Lease{}, false, err
	}
	name, holder, err := normalizeLeaseKey(name, holder)
	if err != nil {
		return Lease{}, false, err
	}
	if err := validateLeaseTTL(ttl); err != nil {
		return Lease{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	current, exists := s.leases[name]
	if exists && !current.ExpiresAt.Before(now) {
		// Held and not expired: no takeover and no holder self-reacquire.
		return current, false, nil
	}
	fence := uint64(1)
	if exists {
		fence = current.Fence + 1
	}
	lease := Lease{
		Name:       name,
		Holder:     holder,
		Fence:      fence,
		AcquiredAt: now,
		RenewedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}
	s.leases[name] = lease
	return lease, true, nil
}

func (s *MemoryStore) RenewLease(ctx context.Context, name, holder string, fence uint64, ttl time.Duration) (Lease, bool, error) {
	if err := checkContext(ctx); err != nil {
		return Lease{}, false, err
	}
	name, holder, err := normalizeLeaseKey(name, holder)
	if err != nil {
		return Lease{}, false, err
	}
	if err := validateLeaseTTL(ttl); err != nil {
		return Lease{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.leases[name]
	if !exists {
		return Lease{}, false, nil
	}
	if current.Holder != holder || current.Fence != fence {
		return current, false, nil
	}
	now := time.Now().UTC()
	current.RenewedAt = now
	current.ExpiresAt = now.Add(ttl)
	s.leases[name] = current
	return current, true, nil
}

func (s *MemoryStore) ReleaseLease(ctx context.Context, name, holder string, fence uint64) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	name, holder, err := normalizeLeaseKey(name, holder)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.leases[name]
	if exists && current.Holder == holder && current.Fence == fence {
		delete(s.leases, name)
	}
	return nil
}

func (s *MemoryStore) Leases(ctx context.Context) ([]Lease, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	leases := make([]Lease, 0, len(s.leases))
	for _, lease := range s.leases {
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool {
		return leases[i].Name < leases[j].Name
	})
	return leases, nil
}
