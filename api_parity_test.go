// Package ovr_test contains a compile-time parity test that references every
// documented v0.1 public symbol from docs/api.md so that any rename, removal,
// or signature drift breaks `go build ./...` and public API drift is
// observable from CI.
//
// The test does not exercise behavior — it only forces type-checking against
// the public API surface. Behavioural coverage lives in dedicated tests.
//
// SPEC GAPS (none right now): every symbol enumerated in this file resolves
// against the package as of the time of writing. When the spec describes a
// public surface that is intentionally not implemented yet, the offending
// reference should be replaced with a `t.Logf("api gap: ...")` line and noted
// in this header block. As of 2026-05 every documented v0.1 primitive listed
// in docs/api.md has a public counterpart.
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

	// Trigger primitives.
	_ = ovr.From
	_ = ovr.Cron("0 6 * * *")
	_ = ovr.Webhook("github")
	_ = ovr.Stream("kafka://broker:9092/topic")
	_ = ovr.IdempotencyKey("Idempotency-Key")
	_ = ovr.VerifySignature("STRIPE_SIGNING_SECRET", "Stripe-Signature")
	_ = ovr.WorkerPool(4)
	_ = ovr.StreamDLQ("kafka://broker:9092/topic.dlq", 5)
	_ = ovr.StreamMaxInFlight(8)

	// Pipe option surface.
	_ = ovr.Pipe
	_ = ovr.Model("anthropic/claude-sonnet-4-6")
	_ = ovr.Fallback("anthropic/claude-sonnet-4-6", "openai/gpt-4.1-mini")
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

	// Output node primitives.
	_ = ovr.Reply
	_ = ovr.Push
	_ = ovr.Sink
	_ = ovr.JSON[parityReply]()
	_ = ovr.SSE()
	_ = ovr.Accepted()
	_ = ovr.Queue("nats://127.0.0.1:4222/results")
	_ = ovr.Queue("kafka://broker:9092/results")
	_ = ovr.Queue("redis://127.0.0.1:6379/results")
	_ = ovr.Queue("sqs://sqs.us-east-1.amazonaws.com/123456789012/results")
	_ = ovr.Log()
	_ = ovr.File("./out/result.json")

	// Composition.
	_ = ovr.Parallel
	_ = ovr.Map
	_ = ovr.Concurrency(8)

	// Capabilities and advanced surfaces.
	_ = ovr.Bash(ovr.Sandbox("/tmp/ouvrier-parity-workspace"))
	_ = ovr.BashTimeout("5s")
	_ = ovr.BashMaxOutputBytes(64 * 1024)
	_ = ovr.UnsafeBashHostExecution()
	_ = ovr.Sandbox("/tmp/ouvrier-parity-workspace")
	_ = ovr.AllowEnv("PATH", "HOME")
	_ = ovr.RequireEnv
	_ = ovr.MCP("moodle-mcp")

	// Runner and advanced configuration.
	_ = ovr.NewRunner
	_ = ovr.Run
	_ = ovr.Validate
	_ = ovr.WithPermissionPolicy
	_ = ovr.WithStateStore
	_ = ovr.MemoryRecord{}
	// StateStore now carries scoped persistent agent memory; reference the
	// method set so signature drift on the public interface breaks the build.
	var memStore ovr.StateStore
	if memStore != nil {
		_ = memStore.SaveMemory
		_ = memStore.Memory
		_ = memStore.ListMemory
	}
	_ = ovr.WithHooks
	_ = ovr.WithSandbox
	_ = ovr.WithSchemaRepairAttempts
	_ = ovr.WithTracer
	_ = ovr.WithOTLPExporter
	_ = ovr.OTLPServiceName
	_ = ovr.OTLPHeaders
	_ = ovr.WithPricing
	_ = ovr.WithProviderBudget
	_ = ovr.PerMillion
	_ = ovr.PricingTable{"anthropic/claude-sonnet-4-6": ovr.ModelRate{}}
	_ = ovr.NopTracer()

	// Exercise the runner constructor without starting any runtime so the
	// option types — RunnerOption, PermissionPolicy, StateStore, Hooks,
	// SandboxConfig, Tracer — remain reachable in this test.
	runner := ovr.NewRunner(
		ovr.WithPermissionPolicy(ovr.AllowSideEffects("email")),
		ovr.WithPermissionPolicy(ovr.AllowSideEffectTargets("webhook", "https://example.com/result")),
		ovr.WithSandbox(ovr.Sandbox(t.TempDir(), ovr.AllowEnv("PATH"))),
		ovr.WithSchemaRepairAttempts(1),
		ovr.WithTracer(ovr.NopTracer()),
		ovr.WithHooks(ovr.NewHooks()),
	)
	_ = runner
}
