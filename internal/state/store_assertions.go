package state

var _ Store = (*MemoryStore)(nil)
var _ Store = (*SQLiteStore)(nil)
var _ Store = (*PostgresStore)(nil)
