package tools

import (
	"encoding/json"
	"strings"
	"time"

	"ouvrier/internal/policy"
)

type ToolKind string

const (
	ToolKindSubAgent ToolKind = "subagent"
)

type Metadata struct {
	Effect           policy.Effect
	IdempotencyKey   string
	SideEffects      []string
	RequiresApproval bool
	Kind             ToolKind
	PartialOK        bool
	ArgumentName     string
	InputSchema      json.RawMessage
	Timeout          time.Duration
}

type Option func(*Executor)

func WithPermissionPolicy(permissionPolicy policy.PermissionPolicy) Option {
	return func(e *Executor) {
		e.policy = permissionPolicy
	}
}

type RegisterOption func(*registeredTool)

func WithMetadata(metadata Metadata) RegisterOption {
	return func(tool *registeredTool) {
		metadata.Effect = normalizeEffect(metadata.Effect)
		metadata.IdempotencyKey = strings.TrimSpace(metadata.IdempotencyKey)
		metadata.SideEffects = cleanLabels(metadata.SideEffects)
		metadata.ArgumentName = strings.TrimSpace(metadata.ArgumentName)
		metadata.InputSchema = append(json.RawMessage(nil), metadata.InputSchema...)
		if metadata.Timeout < 0 {
			metadata.Timeout = 0
		}
		tool.metadata = metadata
	}
}

func normalizeEffect(effect policy.Effect) policy.Effect {
	if effect == "" {
		return policy.EffectSideEffecting
	}
	return effect
}

func cleanLabels(labels []string) []string {
	cleaned := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			cleaned = append(cleaned, label)
		}
	}
	return cleaned
}
