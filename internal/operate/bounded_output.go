package operate

import (
	"fmt"
	"math"
	"sync"
)

const maxAuditStreamBytes = 64 << 10

// boundedOutput drains its producer completely while retaining only a fixed
// prefix. Returning len(p) is important: command pipes must keep draining even
// after the diagnostic budget is exhausted.
type boundedOutput struct {
	mu    sync.RWMutex
	limit int
	label string
	data  []byte
	total int64
}

func newBoundedOutput(limit int, label string) *boundedOutput {
	if limit < 0 {
		limit = 0
	}
	return &boundedOutput{limit: limit, label: label}
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	written := len(p)
	if b.total > math.MaxInt64-int64(written) {
		b.total = math.MaxInt64
	} else {
		b.total += int64(written)
	}
	remaining := b.limit - len(b.data)
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		b.data = append(b.data, p[:remaining]...)
	}
	return written, nil
}

func (b *boundedOutput) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	text := string(b.data)
	if b.total > int64(len(b.data)) {
		text += fmt.Sprintf("\n[%s truncated: retained %d of %d bytes]", b.label, len(b.data), b.total)
	}
	return text
}

func (b *boundedOutput) Truncated() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.total > int64(len(b.data))
}
