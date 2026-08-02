package state

// GloballyAllocatedEventIDStore marks built-in stores whose AddEvent method
// atomically allocates an ID when Event.ID is zero. The unexported marker
// deliberately prevents custom/public stores from being opted into a stronger
// contract they may not implement; those stores retain the legacy
// EventStream-first path for source compatibility.
type GloballyAllocatedEventIDStore interface {
	Store
	globallyAllocatedEventIDs()
}

func (*MemoryStore) globallyAllocatedEventIDs()   {}
func (*SQLiteStore) globallyAllocatedEventIDs()   {}
func (*PostgresStore) globallyAllocatedEventIDs() {}
