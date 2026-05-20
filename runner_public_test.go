package ovr_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"ouvrier"
)

func TestAllowSideEffectsPublicPolicyAllowsExplicitLabel(t *testing.T) {
	permissionPolicy := ovr.AllowSideEffects("email")

	decision, err := permissionPolicy.Authorize(context.Background(), ovr.PermissionAction{
		Kind:        ovr.PermissionActionToolCall,
		ToolName:    "send_email",
		Effect:      ovr.EffectSideEffecting,
		SideEffects: []string{"email"},
	})
	if err != nil {
		t.Fatalf("Authorize returned error: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Allowed = false, reason=%q", decision.Reason)
	}
}

func TestNewRunnerWithPermissionPolicyKeepsRunValidation(t *testing.T) {
	runner := ovr.NewRunner(ovr.WithPermissionPolicy(ovr.AllowSideEffects("email")))

	err := runner.Run(":8080", ovr.Pipe("missing trigger", ovr.Model("anthropic/claude-sonnet-4-6")))
	if !errors.Is(err, ovr.ErrFirstNodeNotFrom) {
		t.Fatalf("Run error = %v, want ErrFirstNodeNotFrom", err)
	}
}

func TestRunnerRejectsNilPermissionPolicy(t *testing.T) {
	runner := ovr.NewRunner(ovr.WithPermissionPolicy(nil))

	err := runner.Run(
		"127.0.0.1:bad-port",
		ovr.From("GET /health"),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want invalid runner option")
	}
	if !strings.Contains(err.Error(), "permission policy is required") {
		t.Fatalf("Run error = %v, want permission policy context", err)
	}
}
