// Package ovr_test contains a compile-time parity test that references every
// documented v0.1 public symbol from specs.md so that any rename, removal, or
// signature drift breaks `go build ./...` and the closing of v0.1 issue #22 is
// observable from CI.
//
// The test does not exercise behavior — it only forces type-checking against
// the public API surface. Behavioural coverage lives in dedicated tests.
//
// SPEC GAPS (none right now): every symbol enumerated in this file resolves
// against the package as of the time of writing. When the spec describes a
// public surface that is intentionally not implemented yet, the offending
// reference should be replaced with a `t.Logf("spec gap: ...")` line and noted
// in this header block. As of 2026-05 every documented v0.1 primitive listed
// in specs.md sections 3, 5, 7, 8, 10, 11 and 15 has a public counterpart.
package ovr_test

import (
	"context"
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

// parityReply is a tiny typed result used by JSON[T] and Output[T] references.
type parityReply struct {
	Status string `json:"status"`
}

// parityTool keeps the Tool() signature exercise free of nil-function failures.
func parityTool(_ context.Context) error { return nil }

// TestPublicV01APIParityCompiles is a no-op test whose purpose is to fail the
// build when any documented v0.1 public symbol is removed or renamed.
//
// Each reference is an assignment to the blank identifier or a call that only
// produces ignored values. Validation errors at runtime are tolerated: the
// goal is type-checking, not behaviour.
func TestPublicV01APIParityCompiles(t *testing.T) {
	t.Helper()

	// Trigger primitives (specs.md §3.1).
	_ = ovr.From
	_ = ovr.Cron("0 6 * * *")
	_ = ovr.Webhook("github")
	_ = ovr.Stream("kafka://broker:9092/topic")
	_ = ovr.IdempotencyKey("Idempotency-Key")
	_ = ovr.VerifySignature("STRIPE_SIGNING_SECRET", "Stripe-Signature")
	_ = ovr.WorkerPool(4)

	// Pipe option surface (specs.md §3.2, §5.1).
	_ = ovr.Pipe
	_ = ovr.Model("anthropic/claude-sonnet-4-6")
	_ = ovr.Timeout("30s")
	_ = ovr.MaxTokens(500_000)
	_ = ovr.MaxCostUSD(5.0)
	_ = ovr.NoCache()
	_ = ovr.SequentialTools()
	_ = ovr.Tool("parity_tool", parityTool)
	_ = ovr.ReadOnly()
	_ = ovr.Idempotent("payload.id")
	_ = ovr.SideEffecting("email")
	_ = ovr.RequiresApproval()
	_ = ovr.ToolTimeout("10s")
	_ = ovr.Describe("LLM-facing description")
	_ = ovr.Param("payload", "free-form payload")
	_ = ovr.Skill("triage")
	_ = ovr.Retry(3, ovr.ExponentialBackoff())
	_ = ovr.ExponentialBackoff()
	_ = ovr.MaxParallel(2)
	_ = ovr.PartialOK()
	_ = ovr.SubAgent
	_ = ovr.Pipeline

	// Output node primitives (specs.md §3.4, §5).
	_ = ovr.Reply
	_ = ovr.Push
	_ = ovr.Sink
	_ = ovr.JSON[parityReply]()
	_ = ovr.SSE()
	_ = ovr.Accepted()
	_ = ovr.Queue("nats://127.0.0.1:4222/results")
	_ = ovr.Log()
	_ = ovr.File("./out/result.json")

	// Composition (specs.md §4.2, §4.3).
	_ = ovr.Parallel
	_ = ovr.Map
	_ = ovr.Concurrency(8)

	// Capabilities and advanced surfaces (specs.md §3.2, §5.3, §5.4, §11.4).
	_ = ovr.Bash(ovr.Sandbox("/tmp/ouvrier-parity-workspace"))
	_ = ovr.BashTimeout("5s")
	_ = ovr.BashMaxOutputBytes(64 * 1024)
	_ = ovr.UnsafeBashHostExecution()
	_ = ovr.Sandbox("/tmp/ouvrier-parity-workspace")
	_ = ovr.AllowEnv("PATH", "HOME")
	_ = ovr.RequireEnv
	_ = ovr.MCP("moodle-mcp")

	// Runner and advanced configuration (specs.md §3.3, §2.1.2, §8).
	_ = ovr.NewRunner
	_ = ovr.Run
	_ = ovr.Validate
	_ = ovr.WithPermissionPolicy
	_ = ovr.WithStateStore
	_ = ovr.WithHooks
	_ = ovr.WithSandbox
	_ = ovr.WithSchemaRepairAttempts
	_ = ovr.WithTracer
	_ = ovr.NopTracer()

	// Exercise the runner constructor without starting any runtime so the
	// option types — RunnerOption, PermissionPolicy, StateStore, Hooks,
	// SandboxConfig, Tracer — remain reachable in this test.
	runner := ovr.NewRunner(
		ovr.WithPermissionPolicy(ovr.AllowSideEffects("email")),
		ovr.WithSandbox(ovr.Sandbox(t.TempDir(), ovr.AllowEnv("PATH"))),
		ovr.WithSchemaRepairAttempts(1),
		ovr.WithTracer(ovr.NopTracer()),
		ovr.WithHooks(ovr.NewHooks()),
	)
	_ = runner
}
