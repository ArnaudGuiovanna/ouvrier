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
	ToolKind         string
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

type PolicyOption func(*DefaultPolicy)

type DefaultPolicy struct {
	allowedSideEffects map[string]struct{}
}

func NewDefaultPolicy(options ...PolicyOption) DefaultPolicy {
	policy := DefaultPolicy{allowedSideEffects: make(map[string]struct{})}
	for _, option := range options {
		if option != nil {
			option(&policy)
		}
	}
	return policy
}

func AllowSideEffects(labels ...string) PolicyOption {
	return func(policy *DefaultPolicy) {
		if policy.allowedSideEffects == nil {
			policy.allowedSideEffects = make(map[string]struct{})
		}
		for _, label := range cleanLabels(labels) {
			policy.allowedSideEffects[label] = struct{}{}
		}
	}
}

func (p DefaultPolicy) Authorize(ctx context.Context, action Action) (Decision, error) {
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
		if strings.TrimSpace(action.ToolKind) == "subagent" {
			return Allow("declared subagent call allowed"), nil
		}
		switch normalizeEffect(action.Effect) {
		case EffectReadOnly:
			return Allow("read-only tool call allowed"), nil
		case EffectIdempotent:
			if strings.TrimSpace(action.IdempotencyKey) == "" {
				return Deny("idempotent tool requires an idempotency key"), nil
			}
			return Allow("idempotent tool call allowed"), nil
		case EffectSideEffecting:
			return p.authorizeSideEffectingTool(action), nil
		default:
			return Deny("tool effect is not allowed"), nil
		}
	default:
		return Deny("action kind is not allowed"), nil
	}
}

func (p DefaultPolicy) authorizeSideEffectingTool(action Action) Decision {
	sideEffects := cleanLabels(action.SideEffects)
	if len(sideEffects) == 0 {
		return Deny("side-effecting tool requires explicit side effect labels")
	}
	for _, label := range sideEffects {
		if _, ok := p.allowedSideEffects[label]; !ok {
			return Deny("side effect " + label + " is not allowed")
		}
	}
	return Allow("side-effecting tool call explicitly allowed")
}

func normalizeEffect(effect Effect) Effect {
	if effect == "" {
		return EffectSideEffecting
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
