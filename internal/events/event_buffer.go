package events

// eventBuffer is a fixed-capacity circular buffer. Its owner provides
// synchronization. Logical indices always run from the oldest retained event
// to the newest, independently of the physical wrap point.
type eventBuffer struct {
	entries []Event
	start   int
	size    int
}

func newEventBuffer(capacity int) eventBuffer {
	return eventBuffer{entries: make([]Event, capacity)}
}

func (b *eventBuffer) append(event Event) {
	if b.size < len(b.entries) {
		index := (b.start + b.size) % len(b.entries)
		b.entries[index] = event
		b.size++
		return
	}
	b.entries[b.start] = event
	b.start = (b.start + 1) % len(b.entries)
}

func (b *eventBuffer) len() int {
	return b.size
}

func (b *eventBuffer) capacity() int {
	return len(b.entries)
}

func (b *eventBuffer) each(visit func(int, Event) bool) {
	if visit == nil {
		return
	}
	for logicalIndex := 0; logicalIndex < b.size; logicalIndex++ {
		physicalIndex := (b.start + logicalIndex) % len(b.entries)
		if !visit(logicalIndex, b.entries[physicalIndex]) {
			return
		}
	}
}
