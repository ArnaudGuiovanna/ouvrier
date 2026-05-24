package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/policy"
	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

type recordingPolicy struct {
	action policy.Action
}

func (p *recordingPolicy) Authorize(ctx context.Context, action policy.Action) (policy.Decision, error) {
	p.action = action
	return policy.Deny("blocked by test policy"), nil
}

func TestExecutorChecksPermissionPolicyBeforeToolCall(t *testing.T) {
	called := false
	executor := NewExecutor()
	err := executor.Register("publish", func(ctx context.Context) error {
		called = true
		return nil
	}, WithMetadata(Metadata{RequiresApproval: true}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call_1", Name: "publish"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if called {
		t.Fatal("tool function was called after permission denial")
	}
	if !result.IsError {
		t.Fatal("IsError = false, want permission error result")
	}
	var message string
	if err := json.Unmarshal(result.Content, &message); err != nil {
		t.Fatalf("error content is not a JSON string: %v", err)
	}
	if !strings.Contains(message, "permission denied") {
		t.Fatalf("message = %q, want permission denied", message)
	}
}

func TestExecutorAuditsAllowedPermissionDecisionBeforeToolCall(t *testing.T) {
	var order []string
	var audit PermissionDecisionAudit
	executor := NewExecutor()
	err := executor.Register("lookup", func(ctx context.Context) error {
		order = append(order, "tool")
		return nil
	}, WithMetadata(Metadata{Effect: policy.EffectReadOnly}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	ctx := ContextWithPermissionDecisionObserver(context.Background(), func(ctx context.Context, observed PermissionDecisionAudit) error {
		order = append(order, "audit")
		audit = observed
		return nil
	})

	result, err := executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "lookup"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("IsError = true, content=%s", result.Content)
	}
	if len(order) != 2 || order[0] != "audit" || order[1] != "tool" {
		t.Fatalf("order = %+v, want audit before tool", order)
	}
	if audit.Action.ToolName != "lookup" ||
		audit.Action.ToolCallID != "call_1" ||
		audit.Action.Effect != policy.EffectReadOnly ||
		!audit.Decision.Allowed {
		t.Fatalf("audit = %+v, want allowed lookup decision", audit)
	}
}

func TestExecutorAuditsDeniedPermissionDecisionAndDoesNotCallTool(t *testing.T) {
	called := false
	var audit PermissionDecisionAudit
	executor := NewExecutor()
	err := executor.Register("publish", func(ctx context.Context) error {
		called = true
		return nil
	}, WithMetadata(Metadata{RequiresApproval: true}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	ctx := ContextWithPermissionDecisionObserver(context.Background(), func(ctx context.Context, observed PermissionDecisionAudit) error {
		audit = observed
		return nil
	})

	result, err := executor.Execute(ctx, provider.ToolCall{ID: "call_publish", Name: "publish"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if called {
		t.Fatal("tool function was called after permission denial")
	}
	if !result.IsError {
		t.Fatal("IsError = false, want permission error result")
	}
	if audit.Action.ToolName != "publish" ||
		audit.Action.ToolCallID != "call_publish" ||
		!audit.Action.RequiresApproval ||
		audit.Decision.Allowed ||
		audit.Decision.Reason == "" {
		t.Fatalf("audit = %+v, want denied publish decision", audit)
	}
}

func TestExecutorBlocksAllowedToolWhenPermissionAuditFails(t *testing.T) {
	boom := errors.New("audit unavailable")
	called := false
	executor := NewExecutor()
	err := executor.Register("lookup", func(ctx context.Context) error {
		called = true
		return nil
	}, WithMetadata(Metadata{Effect: policy.EffectReadOnly}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	ctx := ContextWithPermissionDecisionObserver(context.Background(), func(ctx context.Context, audit PermissionDecisionAudit) error {
		return boom
	})

	_, err = executor.Execute(ctx, provider.ToolCall{ID: "call_1", Name: "lookup"})
	if !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v, want audit error", err)
	}
	if called {
		t.Fatal("tool function was called after audit failure")
	}
}

func TestExecutorPassesToolMetadataToPermissionPolicy(t *testing.T) {
	policyRecorder := &recordingPolicy{}
	executor := NewExecutor(WithPermissionPolicy(policyRecorder))
	err := executor.Register("lookup", func(ctx context.Context) error {
		return nil
	}, WithMetadata(Metadata{
		Effect:         policy.EffectIdempotent,
		IdempotencyKey: "ticket.id",
		SideEffects:    []string{"ticket-write"},
	}))
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result, err := executor.Execute(context.Background(), provider.ToolCall{ID: "call_1", Name: "lookup"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want denied result")
	}
	action := policyRecorder.action
	if action.Kind != policy.ActionToolCall || action.ToolName != "lookup" {
		t.Fatalf("action = %+v, want lookup tool call", action)
	}
	if action.Effect != policy.EffectIdempotent || action.IdempotencyKey != "ticket.id" {
		t.Fatalf("action metadata = %+v", action)
	}
	if len(action.SideEffects) != 1 || action.SideEffects[0] != "ticket-write" {
		t.Fatalf("action side effects = %+v, want ticket-write", action.SideEffects)
	}
}

func TestExecutorNewScopeKeepsPolicyWithoutSharingRegisteredTools(t *testing.T) {
	policyRecorder := &recordingPolicy{}
	executor := NewExecutor(WithPermissionPolicy(policyRecorder))
	if err := executor.Register("base_only", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	scoped := executor.NewScope()
	if _, err := scoped.Execute(context.Background(), provider.ToolCall{ID: "call_missing", Name: "base_only"}); err == nil {
		t.Fatal("scoped Execute returned nil error for base-only tool")
	}
	if err := scoped.Register("scoped", func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	result, err := scoped.Execute(context.Background(), provider.ToolCall{ID: "call_scoped", Name: "scoped"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !result.IsError {
		t.Fatal("IsError = false, want policy denial from copied policy")
	}
	if policyRecorder.action.ToolName != "scoped" {
		t.Fatalf("policy action = %+v, want scoped tool", policyRecorder.action)
	}
}
