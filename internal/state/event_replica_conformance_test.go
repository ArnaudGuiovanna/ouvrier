package state

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

func assertReplicaEventIDConformance(t *testing.T, replicas []Store) {
	t.Helper()
	const perReplica = 24
	start := make(chan struct{})
	errs := make(chan error, len(replicas)*perReplica)
	var wg sync.WaitGroup
	for replicaIndex, replica := range replicas {
		for eventIndex := 0; eventIndex < perReplica; eventIndex++ {
			wg.Add(1)
			go func(replicaIndex, eventIndex int, replica Store) {
				defer wg.Done()
				<-start
				_, err := replica.AddEvent(context.Background(), events.Event{
					Kind:   events.EventBeforeTool,
					ExecID: fmt.Sprintf("exec_%d", replicaIndex),
					Payload: map[string]any{
						"event": eventIndex,
					},
				})
				errs <- err
			}(replicaIndex, eventIndex, replica)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AddEvent across replicas returned error: %v", err)
		}
	}

	recorded, err := replicas[0].EventsSince(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("EventsSince returned error: %v", err)
	}
	want := len(replicas) * perReplica
	if len(recorded) != want {
		t.Fatalf("recorded events = %d, want %d", len(recorded), want)
	}
	for i, event := range recorded {
		if event.ID != uint64(i+1) {
			t.Fatalf("event[%d].ID = %d, want %d", i, event.ID, i+1)
		}
	}
}

func TestMemoryStoreAllocatesEventIDsAcrossReplicaWriters(t *testing.T) {
	store := NewMemoryStore()
	assertReplicaEventIDConformance(t, []Store{store, store})
}

func TestSQLiteStoreAllocatesEventIDsAcrossReplicaConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	replicaA := newTestSQLiteStore(t, path)
	replicaB := newTestSQLiteStore(t, path)
	assertReplicaEventIDConformance(t, []Store{replicaA, replicaB})
}

func TestPostgresStoreAllocatesEventIDsAcrossReplicaPools(t *testing.T) {
	dsn := postgresTestSchemaDSN(t)
	replicaA := openTestPostgresStore(t, dsn)
	replicaB := openTestPostgresStore(t, dsn)
	assertReplicaEventIDConformance(t, []Store{replicaA, replicaB})
}
