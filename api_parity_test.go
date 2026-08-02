// Package ovr_test keeps docs/api.md and the contractual public Go surface in
// bidirectional parity. Compile-time references catch docs -> code drift; the
// AST-backed test catches exported callable code -> docs drift.
//
// The test does not exercise behavior — it only forces type-checking against
// the public API surface. Behavioural coverage lives in dedicated tests.
//
// SPEC GAPS (none right now): every symbol enumerated in this file resolves
// against the package as of the time of writing. When the spec describes a
// public surface that is intentionally not implemented yet, the offending
// reference should be replaced with a `t.Logf("api gap: ...")` line and noted
// in this header block. As of 2026-08 every documented primitive listed
// in docs/api.md has a public counterpart.
package ovr_test

import (
	"context"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

// parityReply is a tiny typed result used by JSON[T] and Output[T] references.
type parityReply struct {
	Status string `json:"status"`
}

// parityTool keeps the Tool() signature exercise free of nil-function failures.
func parityTool(_ context.Context) error { return nil }

// TestPublicAPIParityCompiles is a no-op test whose purpose is to fail the
// build when any documented public symbol is removed or renamed.
//
// Each reference is an assignment to the blank identifier or a call that only
// produces ignored values. Validation errors at runtime are tolerated: the
// goal is type-checking, not behaviour.
func TestPublicAPIParityCompiles(t *testing.T) {
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
	_ = ovr.StreamAckPolicy(ovr.StreamAckManual)
	_ = ovr.StreamAckAuto

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
	var parityProvider ovr.Provider
	_ = parityProvider
	var _ func(ovr.Provider) ovr.RunnerOption = ovr.WithProvider
	var _ func(...ovr.Node) (http.Handler, error) = ovr.Handler
	_ = ovr.NewRunner
	_ = ovr.Run
	_ = ovr.Validate
	_ = ovr.WithPermissionPolicy
	_ = ovr.PermissionDecision{
		Allowed:    false,
		Reason:     "operator approval pending",
		Suspended:  true,
		ApprovalID: "approval-1",
	}
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
	_ = ovr.EventPipelineStarted
	_ = ovr.EventPipelineCompleted
	_ = ovr.EventPipelineFailed
	_ = ovr.EventPipeStarted
	_ = ovr.EventPipeCompleted
	_ = ovr.EventPipeFailed
	_ = ovr.EventSessionStarted
	_ = ovr.EventSessionSaved
	_ = ovr.EventSessionCancelled
	_ = ovr.EventLLMCallStarted
	_ = ovr.EventLLMCallCompleted
	_ = ovr.EventLLMCallFailed
	_ = ovr.EventLLMTokenDelta
	_ = ovr.EventModelFallback
	_ = ovr.EventToolCallStarted
	_ = ovr.EventToolCallCompleted
	_ = ovr.EventToolCallFailed
	_ = ovr.EventPermissionDecision
	_ = ovr.EventIdempotencyDecision
	_ = ovr.EventSignatureDecision
	_ = ovr.EventApprovalRequested
	_ = ovr.EventApprovalApproved
	_ = ovr.EventApprovalDenied
	_ = ovr.EventExecutionSuspended
	_ = ovr.EventExecutionResumed
	_ = ovr.EventHookFailed
	_ = ovr.EventSchemaValidationPassed
	_ = ovr.EventSchemaValidationFailed
	_ = ovr.EventSchemaRepairStarted
	_ = ovr.EventSchemaRepairCompleted
	_ = ovr.EventSchemaRepairFailed
	_ = ovr.EventBudgetExceeded
	_ = ovr.EventTaskStarted
	_ = ovr.EventTaskCompleted
	_ = ovr.EventTaskFailed
	_ = ovr.EventSkillLoaded
	_ = ovr.EventStreamDeadLettered
	_ = ovr.EventStreamRedelivered
	_ = ovr.EventCronLeaseAcquired
	_ = ovr.EventCronLeaseLost
	_ = ovr.EventCronTickSkipped
	_ = ovr.EventDurableRunPruneFailed
	_ = ovr.EventRunRecovered
	_ = ovr.EventRunAbandoned
	_ = ovr.EventReplayIndeterminateTool
	_ = ovr.EventSinkLogged
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
	hooks := ovr.NewHooks()
	var _ func(ovr.EventKind, ovr.Hook) error = hooks.Register
	runner := ovr.NewRunner(
		ovr.WithPermissionPolicy(ovr.AllowSideEffects("email")),
		ovr.WithPermissionPolicy(ovr.AllowSideEffectTargets("webhook", "https://example.com/result")),
		ovr.WithSandbox(ovr.Sandbox("/tmp/ouvrier-parity-workspace", ovr.AllowEnv("PATH"))),
		ovr.WithSchemaRepairAttempts(1),
		ovr.WithTracer(ovr.NopTracer()),
		ovr.WithHooks(hooks),
	)
	var _ func(string, ...ovr.Node) error = runner.Run
	var _ func(...ovr.Node) (http.Handler, error) = runner.Handler
}

// TestPublicAPIReferenceCoversCallableSurface makes parity bidirectional. The
// compile-time references above prove docs -> code; this test proves code ->
// docs for the public callable surface and every exported Ouvrier type exposed
// directly by that surface's signatures.
func TestPublicAPIReferenceCoversCallableSurface(t *testing.T) {
	docs := readDoc(t, "docs/api.md")
	functions, signatureTypes := exportedCallableSurface(t)

	var missing []string
	for _, function := range functions {
		if function.receiver == "" {
			if !documentsFunction(docs, function.name) {
				missing = append(missing, "function "+function.name)
			}
			continue
		}
		qualified := function.receiver + "." + function.name
		if !strings.Contains(docs, qualified) {
			missing = append(missing, "method "+qualified)
		}
	}
	for _, typeName := range signatureTypes {
		if !documentsType(docs, typeName) {
			missing = append(missing, "type "+typeName)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("docs/api.md is missing public contract entries:\n- %s", strings.Join(missing, "\n- "))
	}
}

func TestActiveDocumentationHeadersUseLatestRelease(t *testing.T) {
	for _, path := range []string{
		"docs/api.md",
		"docs/handbook.md",
		"docs/ouvrier-syntax-handbook.md",
	} {
		t.Run(path, func(t *testing.T) {
			lines := strings.Split(readDoc(t, path), "\n")
			if len(lines) > 24 {
				lines = lines[:24]
			}
			header := strings.Join(lines, "\n")
			if !strings.Contains(header, "v0.5.5") {
				t.Fatalf("active documentation header does not identify latest release v0.5.5:\n%s", header)
			}
		})
	}
}

type exportedFunction struct {
	name     string
	receiver string
}

func exportedCallableSurface(t *testing.T) ([]exportedFunction, []string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list public package: %v", err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		matches, matchErr := build.Default.MatchFile(".", name)
		if matchErr != nil {
			t.Fatalf("evaluate build constraints for %s: %v", name, matchErr)
		}
		if !matches {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse public package file %s: %v", name, parseErr)
		}
		if file.Name.Name == "ovr" {
			files[name] = file
		}
	}
	if len(files) == 0 {
		t.Fatal("public package ovr not found")
	}

	exportedTypes := make(map[string]struct{})
	for _, file := range files {
		for _, declaration := range file.Decls {
			generic, ok := declaration.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if ok && typeSpec.Name.IsExported() {
					exportedTypes[typeSpec.Name.Name] = struct{}{}
				}
			}
		}
	}

	var functions []exportedFunction
	signatureTypes := make(map[string]struct{})
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() {
				continue
			}
			exported := exportedFunction{name: function.Name.Name}
			if function.Recv != nil && len(function.Recv.List) > 0 {
				exported.receiver = receiverTypeName(function.Recv.List[0].Type)
				if !ast.IsExported(exported.receiver) {
					continue
				}
			}
			functions = append(functions, exported)
			ast.Inspect(function.Type, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if ok {
					if _, exported := exportedTypes[identifier.Name]; exported {
						signatureTypes[identifier.Name] = struct{}{}
					}
				}
				return true
			})
		}
	}

	types := make([]string, 0, len(signatureTypes))
	for typeName := range signatureTypes {
		types = append(types, typeName)
	}
	sort.Strings(types)
	return functions, types
}

func receiverTypeName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return receiverTypeName(expression.X)
	default:
		return ""
	}
}

func documentsFunction(docs, name string) bool {
	pattern := `\b` + regexp.QuoteMeta(name) + `(?:\[[^]\n]+\])?\s*\(`
	return regexp.MustCompile(pattern).MatchString(docs)
}

func documentsType(docs, name string) bool {
	identifier := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	inFence := false
	for _, line := range strings.Split(docs, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence && identifier.MatchString(line) {
			return true
		}
		if strings.HasPrefix(trimmed, "#") && identifier.MatchString(trimmed) {
			return true
		}
		parts := strings.Split(line, "`")
		for index := 1; index < len(parts); index += 2 {
			if identifier.MatchString(parts[index]) {
				return true
			}
		}
	}
	return strings.Contains(docs, "ovr."+name)
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
