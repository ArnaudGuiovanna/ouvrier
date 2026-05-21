package policy

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultPolicyAllowsSafeToolCalls(t *testing.T) {
	policy := NewDefaultPolicy()

	tests := []struct {
		name   string
		action Action
	}{
		{
			name: "read only",
			action: Action{
				Kind:     ActionToolCall,
				ToolName: "lookup",
				Effect:   EffectReadOnly,
			},
		},
		{
			name: "idempotent with key",
			action: Action{
				Kind:           ActionToolCall,
				ToolName:       "send_email",
				Effect:         EffectIdempotent,
				IdempotencyKey: "email.id",
			},
		},
		{
			name: "declared subagent",
			action: Action{
				Kind:     ActionToolCall,
				ToolName: "translate",
				ToolKind: "subagent",
				Effect:   EffectSideEffecting,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := policy.Authorize(context.Background(), tt.action)
			if err != nil {
				t.Fatalf("Authorize returned error: %v", err)
			}
			if !decision.Allowed {
				t.Fatalf("Allowed = false, reason=%q", decision.Reason)
			}
		})
	}
}

func TestDefaultPolicyDeniesUnsafeToolCalls(t *testing.T) {
	policy := NewDefaultPolicy()

	tests := []struct {
		name       string
		action     Action
		wantReason string
	}{
		{
			name: "default side effecting without label",
			action: Action{
				Kind:     ActionToolCall,
				ToolName: "send_email",
			},
			wantReason: "side effect labels",
		},
		{
			name: "side effecting label not allowed",
			action: Action{
				Kind:        ActionToolCall,
				ToolName:    "send_email",
				Effect:      EffectSideEffecting,
				SideEffects: []string{"email"},
			},
			wantReason: "email",
		},
		{
			name: "idempotent without key",
			action: Action{
				Kind:     ActionToolCall,
				ToolName: "send_email",
				Effect:   EffectIdempotent,
			},
			wantReason: "idempotency key",
		},
		{
			name: "approval gated",
			action: Action{
				Kind:             ActionToolCall,
				ToolName:         "send_email",
				Effect:           EffectSideEffecting,
				SideEffects:      []string{"email"},
				RequiresApproval: true,
			},
			wantReason: "approval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := policy.Authorize(context.Background(), tt.action)
			if err != nil {
				t.Fatalf("Authorize returned error: %v", err)
			}
			if decision.Allowed {
				t.Fatal("Allowed = true, want false")
			}
			if !strings.Contains(decision.Reason, tt.wantReason) {
				t.Fatalf("Reason = %q, want context %q", decision.Reason, tt.wantReason)
			}
		})
	}
}

func TestDefaultPolicyAllowsExplicitSideEffects(t *testing.T) {
	policy := NewDefaultPolicy(AllowSideEffects("email"))

	decision, err := policy.Authorize(context.Background(), Action{
		Kind:        ActionToolCall,
		ToolName:    "send_email",
		Effect:      EffectSideEffecting,
		SideEffects: []string{"email"},
	})
	if err != nil {
		t.Fatalf("Authorize returned error: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Allowed = false, reason=%q", decision.Reason)
	}
}

func TestDefaultPolicyRequiresEverySideEffectToBeAllowed(t *testing.T) {
	policy := NewDefaultPolicy(AllowSideEffects("email"))

	decision, err := policy.Authorize(context.Background(), Action{
		Kind:        ActionToolCall,
		ToolName:    "notify_and_write",
		Effect:      EffectSideEffecting,
		SideEffects: []string{"email", "filesystem"},
	})
	if err != nil {
		t.Fatalf("Authorize returned error: %v", err)
	}
	if decision.Allowed {
		t.Fatal("Allowed = true, want false")
	}
	if !strings.Contains(decision.Reason, "filesystem") {
		t.Fatalf("Reason = %q, want filesystem context", decision.Reason)
	}
}
