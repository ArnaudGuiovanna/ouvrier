package ovr

import (
	"context"

	internalpolicy "github.com/ArnaudGuiovanna/ouvrier/internal/policy"
)

// PermissionActionKind identifies an action that a runner policy can authorize.
type PermissionActionKind string

const (
	PermissionActionToolCall    PermissionActionKind = "tool_call"
	PermissionActionPushWebhook PermissionActionKind = "push_webhook"
	PermissionActionPushQueue   PermissionActionKind = "push_queue"
	PermissionActionSinkFile    PermissionActionKind = "sink_file"
	PermissionActionSinkLog     PermissionActionKind = "sink_log"
)

// Effect describes the execution safety class declared on a Tool.
type Effect string

const (
	EffectReadOnly      Effect = "read_only"
	EffectIdempotent    Effect = "idempotent"
	EffectSideEffecting Effect = "side_effecting"
)

// PermissionAction is the public, auditable action passed to a PermissionPolicy.
type PermissionAction struct {
	Kind             PermissionActionKind
	ToolName         string
	ToolCallID       string
	ToolKind         string
	Target           string
	Effect           Effect
	IdempotencyKey   string
	SideEffects      []string
	RequiresApproval bool
}

// PermissionDecision is the policy verdict for one action.
type PermissionDecision struct {
	Allowed    bool
	Reason     string
	Suspended  bool
	ApprovalID string
}

// PermissionPolicy authorizes privileged runner actions.
type PermissionPolicy interface {
	Authorize(context.Context, PermissionAction) (PermissionDecision, error)
}

type defaultPermissionPolicy struct {
	internal internalpolicy.PermissionPolicy
}

// AllowSideEffects returns the default production policy plus explicit
// non-idempotent side-effect allowances for non-targeted tool calls. Targeted
// actions such as Push, file Sink, MCP, and Bash require AllowSideEffectTargets.
func AllowSideEffects(labels ...string) PermissionPolicy {
	return defaultPermissionPolicy{
		internal: internalpolicy.NewDefaultPolicy(internalpolicy.AllowSideEffects(labels...)),
	}
}

// AllowSideEffectTargets returns the default production policy plus an explicit
// target-scoped allowance for non-idempotent output actions such as Push and
// file Sink. Use "*" only when intentionally allowing every target for a label.
func AllowSideEffectTargets(label string, targets ...string) PermissionPolicy {
	return defaultPermissionPolicy{
		internal: internalpolicy.NewDefaultPolicy(internalpolicy.AllowSideEffectTargets(label, targets...)),
	}
}

func (p defaultPermissionPolicy) Authorize(ctx context.Context, action PermissionAction) (PermissionDecision, error) {
	decision, err := p.internal.Authorize(ctx, internalPermissionAction(action))
	if err != nil {
		return PermissionDecision{}, err
	}
	return publicPermissionDecision(decision), nil
}

type internalPermissionPolicyAdapter struct {
	public PermissionPolicy
}

func (p internalPermissionPolicyAdapter) Authorize(ctx context.Context, action internalpolicy.Action) (internalpolicy.Decision, error) {
	decision, err := p.public.Authorize(ctx, publicPermissionAction(action))
	if err != nil {
		return internalpolicy.Decision{}, err
	}
	return internalPermissionDecision(decision), nil
}

func publicPermissionAction(action internalpolicy.Action) PermissionAction {
	return PermissionAction{
		Kind:             PermissionActionKind(action.Kind),
		ToolName:         action.ToolName,
		ToolCallID:       action.ToolCallID,
		ToolKind:         action.ToolKind,
		Target:           action.Target,
		Effect:           Effect(action.Effect),
		IdempotencyKey:   action.IdempotencyKey,
		SideEffects:      append([]string(nil), action.SideEffects...),
		RequiresApproval: action.RequiresApproval,
	}
}

func internalPermissionAction(action PermissionAction) internalpolicy.Action {
	return internalpolicy.Action{
		Kind:             internalpolicy.ActionKind(action.Kind),
		ToolName:         action.ToolName,
		ToolCallID:       action.ToolCallID,
		ToolKind:         action.ToolKind,
		Target:           action.Target,
		Effect:           internalpolicy.Effect(action.Effect),
		IdempotencyKey:   action.IdempotencyKey,
		SideEffects:      append([]string(nil), action.SideEffects...),
		RequiresApproval: action.RequiresApproval,
	}
}

func publicPermissionDecision(decision internalpolicy.Decision) PermissionDecision {
	return PermissionDecision{
		Allowed:    decision.Allowed,
		Reason:     decision.Reason,
		Suspended:  decision.Suspended,
		ApprovalID: decision.ApprovalID,
	}
}

func internalPermissionDecision(decision PermissionDecision) internalpolicy.Decision {
	return internalpolicy.Decision{
		Allowed:    decision.Allowed,
		Reason:     decision.Reason,
		Suspended:  decision.Suspended,
		ApprovalID: decision.ApprovalID,
	}
}
