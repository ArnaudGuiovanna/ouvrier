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
	ActionToolCall    ActionKind = "tool_call"
	ActionPushWebhook ActionKind = "push_webhook"
	ActionPushQueue   ActionKind = "push_queue"
	ActionSinkFile    ActionKind = "sink_file"
	ActionSinkLog     ActionKind = "sink_log"
)

type Action struct {
	Kind             ActionKind
	ToolName         string
	ToolCallID       string
	ToolKind         string
	Target           string
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
	allowedSideEffects       map[string]struct{}
	allowedSideEffectTargets map[string]map[string]struct{}
}

func NewDefaultPolicy(options ...PolicyOption) DefaultPolicy {
	policy := DefaultPolicy{
		allowedSideEffects:       make(map[string]struct{}),
		allowedSideEffectTargets: make(map[string]map[string]struct{}),
	}
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

func AllowSideEffectTargets(label string, targets ...string) PolicyOption {
	return func(policy *DefaultPolicy) {
		label = strings.TrimSpace(label)
		if label == "" {
			return
		}
		if policy.allowedSideEffectTargets == nil {
			policy.allowedSideEffectTargets = make(map[string]map[string]struct{})
		}
		allowed := policy.allowedSideEffectTargets[label]
		if allowed == nil {
			allowed = make(map[string]struct{})
			policy.allowedSideEffectTargets[label] = allowed
		}
		for _, target := range cleanTargets(targets) {
			allowed[target] = struct{}{}
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
	case ActionPushWebhook:
		return p.authorizeSideEffectingAction(action, "webhook push"), nil
	case ActionPushQueue:
		return p.authorizeSideEffectingAction(action, "queue push"), nil
	case ActionSinkFile:
		return p.authorizeSideEffectingAction(action, "file sink"), nil
	case ActionSinkLog:
		return Allow("redacted log sink allowed"), nil
	default:
		return Deny("action kind is not allowed"), nil
	}
}

func (p DefaultPolicy) authorizeSideEffectingTool(action Action) Decision {
	return p.authorizeSideEffectingAction(action, "side-effecting tool")
}

func (p DefaultPolicy) authorizeSideEffectingAction(action Action, label string) Decision {
	sideEffects := cleanLabels(action.SideEffects)
	if len(sideEffects) == 0 {
		return Deny(label + " requires explicit side effect labels")
	}
	for _, label := range sideEffects {
		if actionRequiresTargetScope(action) {
			target := strings.TrimSpace(action.Target)
			if target == "" {
				return Deny("side effect " + label + " requires a target")
			}
			if !p.targetAllowed(label, target) {
				return Deny("side effect " + label + " target is not allowed")
			}
			continue
		}
		if _, ok := p.allowedSideEffects[label]; !ok {
			return Deny("side effect " + label + " is not allowed")
		}
	}
	return Allow(label + " explicitly allowed")
}

func (p DefaultPolicy) targetAllowed(label, target string) bool {
	allowed := p.allowedSideEffectTargets[label]
	if len(allowed) == 0 {
		return false
	}
	if _, ok := allowed[target]; ok {
		return true
	}
	_, ok := allowed["*"]
	return ok
}

func actionRequiresTargetScope(action Action) bool {
	switch action.Kind {
	case ActionPushWebhook, ActionPushQueue, ActionSinkFile:
		return true
	case ActionToolCall:
		return strings.TrimSpace(action.Target) != ""
	default:
		return false
	}
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

func cleanTargets(targets []string) []string {
	cleaned := make([]string, 0, len(targets))
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target != "" {
			cleaned = append(cleaned, target)
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
