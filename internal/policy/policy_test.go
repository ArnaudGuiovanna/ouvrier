package policy

import (
	"context"
	"testing"
)

func TestDefaultPolicyAllowsClassifiedToolCalls(t *testing.T) {
	policy := NewDefaultPolicy()

	tests := []struct {
		name   string
		effect Effect
	}{
		{name: "read only", effect: EffectReadOnly},
		{name: "idempotent", effect: EffectIdempotent},
		{name: "side effecting", effect: EffectSideEffecting},
		{name: "default side effecting", effect: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := policy.Authorize(context.Background(), Action{
				Kind:     ActionToolCall,
				ToolName: "lookup",
				Effect:   tt.effect,
			})
			if err != nil {
				t.Fatalf("Authorize returned error: %v", err)
			}
			if !decision.Allowed {
				t.Fatalf("Allowed = false, reason=%q", decision.Reason)
			}
		})
	}
}

func TestDefaultPolicyDeniesApprovalGatedToolCalls(t *testing.T) {
	policy := NewDefaultPolicy()

	decision, err := policy.Authorize(context.Background(), Action{
		Kind:             ActionToolCall,
		ToolName:         "send_email",
		Effect:           EffectSideEffecting,
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("Authorize returned error: %v", err)
	}
	if decision.Allowed {
		t.Fatal("Allowed = true, want false")
	}
	if decision.Reason == "" {
		t.Fatal("Reason is empty")
	}
}
