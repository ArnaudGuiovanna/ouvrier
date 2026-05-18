package policy

import (
	"context"
	"errors"
	"strings"
)

var ErrDenied = errors.New("permission denied")

type Effect string

const (
	EffectReadOnly      Effect = "read_only"
	EffectIdempotent    Effect = "idempotent"
	EffectSideEffecting Effect = "side_effecting"
)

type ActionKind string

const (
	ActionToolCall ActionKind = "tool_call"
)

type Action struct {
	Kind             ActionKind
	ToolName         string
	ToolCallID       string
	Effect           Effect
	IdempotencyKey   string
	SideEffects      []string
	RequiresApproval bool
}

type Decision struct {
	Allowed bool
	Reason  string
}

type PermissionPolicy interface {
	Authorize(ctx context.Context, action Action) (Decision, error)
}

type DefaultPolicy struct{}

func NewDefaultPolicy() DefaultPolicy {
	return DefaultPolicy{}
}

func (DefaultPolicy) Authorize(ctx context.Context, action Action) (Decision, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}

	switch action.Kind {
	case ActionToolCall:
		if strings.TrimSpace(action.ToolName) == "" {
			return Deny("tool name is required"), nil
		}
		if action.RequiresApproval {
			return Deny("tool requires explicit approval"), nil
		}
		return Allow("tool call allowed"), nil
	default:
		return Deny("action kind is not allowed"), nil
	}
}

func Allow(reason string) Decision {
	return Decision{Allowed: true, Reason: strings.TrimSpace(reason)}
}

func Deny(reason string) Decision {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = ErrDenied.Error()
	}
	return Decision{Allowed: false, Reason: reason}
}
