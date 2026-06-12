package ovr_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier"
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

func TestAllowSideEffectsPublicPolicyAllowsQueuePush(t *testing.T) {
	target := "nats://127.0.0.1:4222/tickets"
	permissionPolicy := ovr.AllowSideEffectTargets("queue", target)

	decision, err := permissionPolicy.Authorize(context.Background(), ovr.PermissionAction{
		Kind:        ovr.PermissionActionPushQueue,
		Target:      target,
		Effect:      ovr.EffectSideEffecting,
		SideEffects: []string{"queue"},
	})
	if err != nil {
		t.Fatalf("Authorize returned error: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Allowed = false, reason=%q", decision.Reason)
	}
}

func TestAllowSideEffectsPublicPolicyDoesNotAllowTargetBlindQueuePush(t *testing.T) {
	permissionPolicy := ovr.AllowSideEffects("queue")

	decision, err := permissionPolicy.Authorize(context.Background(), ovr.PermissionAction{
		Kind:        ovr.PermissionActionPushQueue,
		Target:      "nats://127.0.0.1:4222/tickets",
		Effect:      ovr.EffectSideEffecting,
		SideEffects: []string{"queue"},
	})
	if err != nil {
		t.Fatalf("Authorize returned error: %v", err)
	}
	if decision.Allowed {
		t.Fatal("Allowed = true, want target-scoped denial")
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

func TestRunnerRejectsNilStateStore(t *testing.T) {
	runner := ovr.NewRunner(ovr.WithStateStore(nil))

	err := runner.Run(
		"127.0.0.1:bad-port",
		ovr.From("GET /health"),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want invalid runner option")
	}
	if !strings.Contains(err.Error(), "state store is required") {
		t.Fatalf("Run error = %v, want state store context", err)
	}
}

func TestNewRunnerAcceptsSandboxAllowEnv(t *testing.T) {
	runner := ovr.NewRunner(ovr.WithSandbox(ovr.Sandbox(t.TempDir(), ovr.AllowEnv("PATH"))))

	err := runner.Run(
		"127.0.0.1:bad-port",
		ovr.From("GET /health"),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want invalid address after accepting sandbox config")
	}
	if strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("Run error = %v, want sandbox config accepted", err)
	}
}

func TestRunnerRejectsNilHooks(t *testing.T) {
	runner := ovr.NewRunner(ovr.WithHooks(nil))

	err := runner.Run(
		"127.0.0.1:bad-port",
		ovr.From("GET /health"),
		ovr.Reply(ovr.JSON[testReply]()),
	)
	if err == nil {
		t.Fatal("Run returned nil, want invalid runner option")
	}
	if !strings.Contains(err.Error(), "hooks are required") {
		t.Fatalf("Run error = %v, want hooks context", err)
	}
}

func TestPublicHooksRejectInvalidRegistration(t *testing.T) {
	hooks := ovr.NewHooks()
	if err := hooks.Register("", func(ctx context.Context, event ovr.Event) (ovr.Event, error) {
		return event, nil
	}); err == nil {
		t.Fatal("Register returned nil for empty event kind")
	}
	if err := hooks.Register(ovr.EventPipelineStarted, nil); err == nil {
		t.Fatal("Register returned nil for nil hook")
	}
}

func TestPublicHookFailedEventKind(t *testing.T) {
	if ovr.EventHookFailed != ovr.EventKind("hook_failed") {
		t.Fatalf("EventHookFailed = %q, want hook_failed", ovr.EventHookFailed)
	}
}

func TestPublicEventKindsExposeRuntimeEvents(t *testing.T) {
	cases := map[ovr.EventKind]string{
		ovr.EventLLMTokenDelta:       "llm_token_delta",
		ovr.EventModelFallback:       "model_fallback",
		ovr.EventApprovalRequested:   "approval_requested",
		ovr.EventApprovalApproved:    "approval_approved",
		ovr.EventApprovalDenied:      "approval_denied",
		ovr.EventExecutionSuspended:  "execution_suspended",
		ovr.EventExecutionResumed:    "execution_resumed",
		ovr.EventSkillLoaded:         "skill_loaded",
		ovr.EventStreamDeadLettered:  "stream_dead_lettered",
		ovr.EventStreamRedelivered:   "stream_redelivered",
		ovr.EventPermissionDecision:  "permission_decision",
		ovr.EventIdempotencyDecision: "idempotency_decision",
		ovr.EventSchemaRepairStarted: "schema_repair_started",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Fatalf("event kind = %q, want %q", got, want)
		}
	}
}
