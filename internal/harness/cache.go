package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func (h *Harness) requestCacheKey() string {
	return h.requestCacheKeyFor(h.requestSystemPrompt(), h.tools)
}

func (h *Harness) requestCacheKeyFor(system string, tools []provider.ToolSpec) string {
	if !h.promptCache {
		return ""
	}
	payload := struct {
		Model  string              `json:"model"`
		System string              `json:"system"`
		Tools  []provider.ToolSpec `json:"tools,omitempty"`
	}{
		Model:  h.model,
		System: system,
		Tools:  tools,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "prompt:" + hex.EncodeToString(sum[:])
}
