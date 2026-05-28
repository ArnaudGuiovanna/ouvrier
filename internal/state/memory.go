package state

import (
	"fmt"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/events"
)

// normalizeMemory validates and redaction-cleans a memory write shared by every
// Store backend. It trims the scope/key, enforces the value size bound, and
// applies the same credential redaction used for persisted events so that no
// secret ever reaches durable storage.
func normalizeMemory(scope, key, value string) (string, string, string, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "", "", "", fmt.Errorf("memory scope is required")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", "", fmt.Errorf("memory key is required")
	}
	if len(value) > MaxMemoryValueBytes {
		return "", "", "", fmt.Errorf("memory value exceeds %d bytes", MaxMemoryValueBytes)
	}
	value = events.RedactJSONText(value)
	return scope, key, value, nil
}
