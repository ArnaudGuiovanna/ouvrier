package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// assertLeaseConformance is the shared fenced-lease conformance suite exercised
// by the in-memory, SQLite, and Postgres backends so all implementations stay
// in sync. newStore must return a fresh store on each call; the suite discovers
// the lease capability via the same type assertion the runtime will use.
func assertLeaseConformance(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()

	leaseStore := func(t *testing.T) LeaseStore {
		t.Helper()
		store := newStore(t)
		leases, ok := store.(LeaseStore)
		if !ok {
			t.Fatalf("store %T does not implement LeaseStore", store)
		}
		return leases
	}

	t.Run("FreshAcquireStartsAtFenceOne", func(t *testing.T) {
		store := leaseStore(t)
		lease, acquired, err := store.AcquireLease(context.Background(), "cron:abc:0", "replica-a", time.Minute)
		if err != nil {
			t.Fatalf("AcquireLease returned error: %v", err)
		}
		if !acquired {
			t.Fatal("AcquireLease acquired = false on fresh lease, want true")
		}
		if lease.Name != "cron:abc:0" || lease.Holder != "replica-a" {
			t.Fatalf("lease = %+v, want name/holder round-trip", lease)
		}
		if lease.Fence != 1 {
			t.Fatalf("fresh lease fence = %d, want 1", lease.Fence)
		}
		if lease.AcquiredAt.IsZero() || lease.RenewedAt.IsZero() || lease.ExpiresAt.IsZero() {
			t.Fatalf("lease timestamps not populated: %+v", lease)
		}
		if !lease.ExpiresAt.After(lease.AcquiredAt) {
			t.Fatalf("ExpiresAt %s not after AcquiredAt %s", lease.ExpiresAt, lease.AcquiredAt)
		}
	})

	t.Run("HeldLeaseRejectsContendersAndSelfReacquire", func(t *testing.T) {
		store := leaseStore(t)
		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}

		current, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-b", time.Minute)
		if err != nil {
			t.Fatalf("contender AcquireLease returned error: %v", err)
		}
		if acquired {
			t.Fatal("contender acquired a held lease, want false")
		}
		if current.Holder != held.Holder || current.Fence != held.Fence {
			t.Fatalf("contender observed lease %+v, want current %+v", current, held)
		}

		// No holder self-reacquire clause: the only extension path is Renew.
		current, acquired, err = store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
		if err != nil {
			t.Fatalf("self AcquireLease returned error: %v", err)
		}
		if acquired {
			t.Fatal("holder re-acquired its own held lease, want false")
		}
		if current.Fence != held.Fence {
			t.Fatalf("self re-acquire observed fence %d, want unchanged %d", current.Fence, held.Fence)
		}
	})

	t.Run("SixteenContenderRaceYieldsExactlyOneHolder", func(t *testing.T) {
		store := leaseStore(t)
		const contenders = 16

		type result struct {
			holder   string
			lease    Lease
			acquired bool
			err      error
		}
		start := make(chan struct{})
		results := make(chan result, contenders)
		var wg sync.WaitGroup
		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				holder := fmt.Sprintf("replica-%d", i)
				lease, acquired, err := store.AcquireLease(context.Background(), "race", holder, time.Minute)
				results <- result{holder: holder, lease: lease, acquired: acquired, err: err}
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)

		winners := 0
		winner := ""
		seen := make([]result, 0, contenders)
		for got := range results {
			if got.err != nil {
				t.Fatalf("AcquireLease returned error: %v", got.err)
			}
			seen = append(seen, got)
			if got.acquired {
				winners++
				winner = got.holder
				if got.lease.Fence != 1 {
					t.Fatalf("winner fence = %d, want 1", got.lease.Fence)
				}
			}
		}
		if winners != 1 {
			t.Fatalf("acquire race winners = %d, want exactly 1", winners)
		}
		for _, got := range seen {
			if !got.acquired && got.lease.Holder != winner {
				t.Fatalf("loser observed holder %q, want winner %q", got.lease.Holder, winner)
			}
		}
	})

	t.Run("ExpiryTakeoverIncrementsFence", func(t *testing.T) {
		store := leaseStore(t)
		first, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", 100*time.Millisecond)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}

		second := acquireLeaseEventually(t, store, "lease-1", "replica-b", time.Minute)
		if second.Holder != "replica-b" {
			t.Fatalf("takeover holder = %q, want replica-b", second.Holder)
		}
		if second.Fence != first.Fence+1 {
			t.Fatalf("takeover fence = %d, want %d", second.Fence, first.Fence+1)
		}
	})

	t.Run("FenceStrictlyMonotonicAcrossTakeovers", func(t *testing.T) {
		store := leaseStore(t)
		previous := uint64(0)
		for i := 0; i < 3; i++ {
			holder := fmt.Sprintf("replica-%d", i)
			lease := acquireLeaseEventually(t, store, "lease-1", holder, 80*time.Millisecond)
			if lease.Fence != previous+1 {
				t.Fatalf("takeover %d fence = %d, want strictly monotonic %d", i, lease.Fence, previous+1)
			}
			previous = lease.Fence
		}
	})

	t.Run("RenewExtendsExpiryAndKeepsFence", func(t *testing.T) {
		store := leaseStore(t)
		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", 30*time.Second)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}

		time.Sleep(50 * time.Millisecond) // exceed SQLite's millisecond timestamp precision
		renewed, ok, err := store.RenewLease(context.Background(), "lease-1", "replica-a", held.Fence, 30*time.Second)
		if err != nil {
			t.Fatalf("RenewLease returned error: %v", err)
		}
		if !ok {
			t.Fatal("RenewLease ok = false for matching holder and fence, want true")
		}
		if renewed.Fence != held.Fence {
			t.Fatalf("renewed fence = %d, want unchanged %d", renewed.Fence, held.Fence)
		}
		if renewed.Holder != held.Holder {
			t.Fatalf("renewed holder = %q, want %q", renewed.Holder, held.Holder)
		}
		if !renewed.AcquiredAt.Equal(held.AcquiredAt) {
			t.Fatalf("renew changed AcquiredAt from %s to %s", held.AcquiredAt, renewed.AcquiredAt)
		}
		if !renewed.ExpiresAt.After(held.ExpiresAt) {
			t.Fatalf("renewed ExpiresAt %s not after previous %s", renewed.ExpiresAt, held.ExpiresAt)
		}
		if !renewed.RenewedAt.After(held.RenewedAt) {
			t.Fatalf("renewed RenewedAt %s not after previous %s", renewed.RenewedAt, held.RenewedAt)
		}
	})

	t.Run("RenewSurvivesExpiryUntilTakeover", func(t *testing.T) {
		// An expired lease that no contender has taken over yet can still be
		// renewed: the fence, not wall-clock expiry, is the safety boundary
		// (any takeover bumps the fence and fails this renew instead).
		store := leaseStore(t)
		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", 50*time.Millisecond)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}

		time.Sleep(400 * time.Millisecond)
		renewed, ok, err := store.RenewLease(context.Background(), "lease-1", "replica-a", held.Fence, time.Minute)
		if err != nil {
			t.Fatalf("RenewLease returned error: %v", err)
		}
		if !ok {
			t.Fatal("RenewLease ok = false for expired-but-untouched lease, want true")
		}
		if renewed.Fence != held.Fence {
			t.Fatalf("renewed fence = %d, want unchanged %d", renewed.Fence, held.Fence)
		}
	})

	t.Run("RenewWithStaleFenceFails", func(t *testing.T) {
		store := leaseStore(t)
		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}

		current, ok, err := store.RenewLease(context.Background(), "lease-1", "replica-a", held.Fence+1, time.Minute)
		if err != nil {
			t.Fatalf("RenewLease returned error: %v", err)
		}
		if ok {
			t.Fatal("RenewLease ok = true with stale fence, want false")
		}
		if current.Holder != held.Holder || current.Fence != held.Fence {
			t.Fatalf("failed renew observed %+v, want current %+v", current, held)
		}
	})

	t.Run("RenewAfterTakeoverFails", func(t *testing.T) {
		store := leaseStore(t)
		first, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", 80*time.Millisecond)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}
		second := acquireLeaseEventually(t, store, "lease-1", "replica-b", time.Minute)

		current, ok, err := store.RenewLease(context.Background(), "lease-1", "replica-a", first.Fence, time.Minute)
		if err != nil {
			t.Fatalf("RenewLease returned error: %v", err)
		}
		if ok {
			t.Fatal("RenewLease ok = true after takeover, want false")
		}
		if current.Holder != second.Holder || current.Fence != second.Fence {
			t.Fatalf("failed renew observed %+v, want takeover lease %+v", current, second)
		}
	})

	t.Run("RenewWithWrongHolderFails", func(t *testing.T) {
		store := leaseStore(t)
		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}

		_, ok, err := store.RenewLease(context.Background(), "lease-1", "replica-b", held.Fence, time.Minute)
		if err != nil {
			t.Fatalf("RenewLease returned error: %v", err)
		}
		if ok {
			t.Fatal("RenewLease ok = true for non-holder, want false")
		}
	})

	t.Run("RenewMissingLeaseFails", func(t *testing.T) {
		store := leaseStore(t)
		lease, ok, err := store.RenewLease(context.Background(), "absent", "replica-a", 1, time.Minute)
		if err != nil {
			t.Fatalf("RenewLease returned error: %v", err)
		}
		if ok {
			t.Fatal("RenewLease ok = true for absent lease, want false")
		}
		if lease != (Lease{}) {
			t.Fatalf("RenewLease on absent lease = %+v, want zero Lease", lease)
		}
	})

	t.Run("ReleaseAllowsImmediateReacquire", func(t *testing.T) {
		store := leaseStore(t)
		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}
		if err := store.ReleaseLease(context.Background(), "lease-1", "replica-a", held.Fence); err != nil {
			t.Fatalf("ReleaseLease returned error: %v", err)
		}

		lease, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-b", time.Minute)
		if err != nil {
			t.Fatalf("AcquireLease after release returned error: %v", err)
		}
		if !acquired {
			t.Fatal("AcquireLease after release acquired = false, want immediate re-acquire")
		}
		if lease.Holder != "replica-b" {
			t.Fatalf("re-acquired holder = %q, want replica-b", lease.Holder)
		}
		if lease.Fence != 1 {
			t.Fatalf("re-acquired fence = %d, want 1 (release deletes the row)", lease.Fence)
		}
	})

	t.Run("ReleaseIsIdempotentAndFenceChecked", func(t *testing.T) {
		store := leaseStore(t)
		if err := store.ReleaseLease(context.Background(), "absent", "replica-a", 1); err != nil {
			t.Fatalf("ReleaseLease on absent lease returned error: %v, want idempotent nil", err)
		}

		held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
		if err != nil || !acquired {
			t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
		}
		if err := store.ReleaseLease(context.Background(), "lease-1", "replica-a", held.Fence+1); err != nil {
			t.Fatalf("stale-fence ReleaseLease returned error: %v, want nil no-op", err)
		}
		if err := store.ReleaseLease(context.Background(), "lease-1", "replica-b", held.Fence); err != nil {
			t.Fatalf("wrong-holder ReleaseLease returned error: %v, want nil no-op", err)
		}
		if _, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-b", time.Minute); err != nil || acquired {
			t.Fatalf("lease released by stale fence or wrong holder: acquired=%v err=%v", acquired, err)
		}

		if err := store.ReleaseLease(context.Background(), "lease-1", "replica-a", held.Fence); err != nil {
			t.Fatalf("matching ReleaseLease returned error: %v", err)
		}
		if _, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-b", time.Minute); err != nil || !acquired {
			t.Fatalf("AcquireLease after matching release: acquired=%v err=%v, want true", acquired, err)
		}
	})

	t.Run("LeasesListsAllRowsIncludingExpiredSortedByName", func(t *testing.T) {
		store := leaseStore(t)
		empty, err := store.Leases(context.Background())
		if err != nil {
			t.Fatalf("Leases returned error: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("Leases on fresh store = %+v, want empty", empty)
		}

		if _, acquired, err := store.AcquireLease(context.Background(), "b-lease", "replica-b", time.Minute); err != nil || !acquired {
			t.Fatalf("AcquireLease(b-lease) acquired=%v err=%v", acquired, err)
		}
		if _, acquired, err := store.AcquireLease(context.Background(), "a-lease", "replica-a", 50*time.Millisecond); err != nil || !acquired {
			t.Fatalf("AcquireLease(a-lease) acquired=%v err=%v", acquired, err)
		}
		time.Sleep(300 * time.Millisecond) // let a-lease expire; it must still be listed

		leases, err := store.Leases(context.Background())
		if err != nil {
			t.Fatalf("Leases returned error: %v", err)
		}
		if len(leases) != 2 {
			t.Fatalf("Leases = %d entries, want 2 (expired rows included): %+v", len(leases), leases)
		}
		if leases[0].Name != "a-lease" || leases[1].Name != "b-lease" {
			t.Fatalf("Leases order = [%s %s], want [a-lease b-lease]", leases[0].Name, leases[1].Name)
		}
		if leases[0].Holder != "replica-a" || leases[0].Fence != 1 {
			t.Fatalf("expired lease entry = %+v, want original holder and fence", leases[0])
		}
	})

	t.Run("ValidationErrors", func(t *testing.T) {
		store := leaseStore(t)
		ctx := context.Background()
		if _, _, err := store.AcquireLease(ctx, "  ", "replica-a", time.Minute); err == nil {
			t.Fatal("AcquireLease accepted blank name, want error")
		}
		if _, _, err := store.AcquireLease(ctx, "lease-1", "", time.Minute); err == nil {
			t.Fatal("AcquireLease accepted empty holder, want error")
		}
		if _, _, err := store.AcquireLease(ctx, "lease-1", "replica-a", 0); err == nil {
			t.Fatal("AcquireLease accepted non-positive ttl, want error")
		}
		if _, _, err := store.RenewLease(ctx, "", "replica-a", 1, time.Minute); err == nil {
			t.Fatal("RenewLease accepted empty name, want error")
		}
		if _, _, err := store.RenewLease(ctx, "lease-1", "replica-a", 1, -time.Second); err == nil {
			t.Fatal("RenewLease accepted non-positive ttl, want error")
		}
		if err := store.ReleaseLease(ctx, "", "replica-a", 1); err == nil {
			t.Fatal("ReleaseLease accepted empty name, want error")
		}
		if err := store.ReleaseLease(ctx, "lease-1", " ", 1); err == nil {
			t.Fatal("ReleaseLease accepted blank holder, want error")
		}
	})

	t.Run("HonorsCanceledContext", func(t *testing.T) {
		store := leaseStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := store.AcquireLease(ctx, "lease-1", "replica-a", time.Minute); !errors.Is(err, context.Canceled) {
			t.Fatalf("AcquireLease error = %v, want context.Canceled", err)
		}
		if _, _, err := store.RenewLease(ctx, "lease-1", "replica-a", 1, time.Minute); !errors.Is(err, context.Canceled) {
			t.Fatalf("RenewLease error = %v, want context.Canceled", err)
		}
		if err := store.ReleaseLease(ctx, "lease-1", "replica-a", 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("ReleaseLease error = %v, want context.Canceled", err)
		}
		if _, err := store.Leases(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Leases error = %v, want context.Canceled", err)
		}
	})
}

// acquireLeaseEventually polls AcquireLease until the lease's previous holder
// has expired (by database time) and the new holder wins, failing the test if
// the takeover does not converge.
func acquireLeaseEventually(t *testing.T, store LeaseStore, name, holder string, ttl time.Duration) Lease {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lease, acquired, err := store.AcquireLease(context.Background(), name, holder, ttl)
		if err != nil {
			t.Fatalf("AcquireLease(%q, %q) returned error: %v", name, holder, err)
		}
		if acquired {
			return lease
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lease %q not acquired by %q within deadline", name, holder)
	return Lease{}
}

func TestMemoryStoreLeaseConformance(t *testing.T) {
	assertLeaseConformance(t, func(t *testing.T) Store {
		return NewMemoryStore()
	})
}

func TestSQLiteStoreLeaseConformance(t *testing.T) {
	assertLeaseConformance(t, func(t *testing.T) Store {
		return newTestSQLiteStore(t, t.TempDir()+"/state.db")
	})
}

func TestPostgresStoreLeaseConformance(t *testing.T) {
	assertLeaseConformance(t, func(t *testing.T) Store {
		return newTestPostgresStore(t)
	})
}

func TestSQLiteStoreLeasePersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	store := newTestSQLiteStore(t, path)
	held, acquired, err := store.AcquireLease(context.Background(), "lease-1", "replica-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("AcquireLease acquired=%v err=%v", acquired, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened := newTestSQLiteStore(t, path)
	leases, err := reopened.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	if len(leases) != 1 || leases[0].Holder != "replica-a" || leases[0].Fence != held.Fence {
		t.Fatalf("Leases after reopen = %+v, want persisted lease %+v", leases, held)
	}
	if _, ok, err := reopened.RenewLease(context.Background(), "lease-1", "replica-a", held.Fence, time.Minute); err != nil || !ok {
		t.Fatalf("RenewLease after reopen ok=%v err=%v, want true", ok, err)
	}
}

// TestPostgresStoreLeaseTwoPoolsRace races lease acquisition across two
// independent connection pools against the same schema, simulating two worker
// replicas contending for cron leadership.
func TestPostgresStoreLeaseTwoPoolsRace(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	replicaA := openTestPostgresStore(t, dsn)
	replicaB := openTestPostgresStore(t, dsn)
	replicas := []*PostgresStore{replicaA, replicaB}

	const contenders = 16
	start := make(chan struct{})
	type result struct {
		holder   string
		acquired bool
		err      error
	}
	results := make(chan result, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			holder := fmt.Sprintf("replica-%d", i)
			_, acquired, err := replicas[i%2].AcquireLease(context.Background(), "cron-leader", holder, time.Minute)
			results <- result{holder: holder, acquired: acquired, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	winner := ""
	for got := range results {
		if got.err != nil {
			t.Fatalf("AcquireLease returned error: %v", got.err)
		}
		if got.acquired {
			winners++
			winner = got.holder
		}
	}
	if winners != 1 {
		t.Fatalf("acquire winners across two pools = %d, want exactly 1", winners)
	}

	// Both pools observe the same holder.
	leases, err := replicaB.Leases(context.Background())
	if err != nil {
		t.Fatalf("Leases returned error: %v", err)
	}
	if len(leases) != 1 || leases[0].Holder != winner {
		t.Fatalf("Leases = %+v, want single lease held by %q", leases, winner)
	}
}
