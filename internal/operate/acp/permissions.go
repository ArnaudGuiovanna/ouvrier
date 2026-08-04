package acp

import (
	"encoding/json"
)

func (c *client) permissionAllowed(kind string, rawInput json.RawMessage) bool {
	_ = c
	_ = kind
	_ = rawInput
	// Source context is supplied by Ouvrier. The agent receives no filesystem,
	// process, network-tool, or subagent capability and can only return a
	// structured patch plan for Ouvrier to validate and apply itself.
	return false
}
