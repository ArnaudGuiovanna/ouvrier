package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ouvrier/internal/policy"
	"ouvrier/internal/provider"
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
