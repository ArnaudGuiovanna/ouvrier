# Ouvrier Public API Reference

This file is a single-source map of the public surface in package
`ovr "github.com/ArnaudGuiovanna/ouvrier"`. It mirrors the shipped framework
contract documented in the handbook and is exercised by the
`TestPublicAPIParityCompiles` golden test so the doc cannot drift away from the
code. As of June 2026, `main` contains the v0.2 milestone work; `v0.1.0`
remains the latest tagged release until v0.2 is tagged.

## Triggers

| Function                          | Purpose                                                                  |
| --------------------------------- | ------------------------------------------------------------------------ |
| `From(value any) Node`            | Declares the trigger. Accepts an HTTP route literal (`"POST /tickets"`), a `CronTrigger`, a `WebhookEndpoint`, or a `StreamTrigger`. |
| `Cron(expr string) CronTrigger`   | Cron schedule trigger (`"0 6 * * *"`).                                   |
| `Webhook(provider string) WebhookEndpoint` | Signed inbound webhook (`github`, `stripe`, custom).             |
| `Stream(uri string) StreamTrigger`| Stream subscriber (`kafka://`, `nats://`, `redis://`).                    |

Trigger options (passed inside the trigger constructor or via `From`-level
`FromOption`s):

| Option                                            | Purpose                                              |
| ------------------------------------------------- | ---------------------------------------------------- |
| `IdempotencyKey(header string) FromOption`        | Use the named HTTP/webhook header as the idempotency key. |
| `VerifySignature(envVar, header string) FromOption` | HMAC-SHA256 signature verification using a shared secret read from `envVar`. |
| `WorkerPool(limit int) FromOption`                 | Cap concurrent trigger handlers.                     |
| `StreamDLQ(target string, maxAttempts int) FromOption` | Route a poisoned stream message to a dead-letter target after `maxAttempts` failed deliveries. Broker targets are published over the real queue transport; runtime replay drains the retained runtime DLQ copy for that plan. |
| `StreamMaxInFlight(limit int) FromOption`          | Bound concurrently processed stream messages so a slow handler applies backpressure to the broker. |
| `StreamAckPolicy(policy StreamAckMode) FromOption` | Per-broker acknowledgement mode: `StreamAckAuto` (default; runtime acks after successful processing) or `StreamAckManual` (handler owns the ack, broker redelivers until acked). No-op for brokers whose receiver exposes no ack closure. |

## Pipe

```go
ovr.Pipe(goal string, options ...PipeOption) Node
```

| Option                                  | Purpose                                            |
| --------------------------------------- | -------------------------------------------------- |
| `Model(id string)`                      | Provider/model id (`anthropic/claude-sonnet-4-6`). |
| `Fallback(models ...string)`            | Ordered fallback models tried on classified provider failures. |
| `Timeout(value string)`                 | Wall-clock budget (`"30s"`).                       |
| `MaxTokens(max int)`                    | Token budget.                                      |
| `MaxCostUSD(max float64)`               | Cost budget in USD.                                |
| `NoCache()`                             | Disable provider prompt-cache hints for this Pipe. |
| `SequentialTools()`                     | Run a turn's tool calls one at a time.             |
| `Tool(name string, fn any, opts...)`    | Expose a Go function as a tool.                    |
| `Skill(dir string)`                     | Load a `skills/<dir>/SKILL.md`.                    |
| `Retry(max int, BackoffPolicy?)`        | Provider/transient retries + retry-safe tool retries. |
| `SubAgent(name string, p PipelineSpec, opts...)` | Expose a child pipeline as a governed tool. |
| `Output[T]()`                           | Force the Pipe to produce JSON matching `T`.       |

### Tool options

| Option                                  | Purpose                                            |
| --------------------------------------- | -------------------------------------------------- |
| `ReadOnly()`                            | Side-effect-free.                                  |
| `Idempotent(keyExpr string)`            | Replay-safe by an idempotency key expression.      |
| `SideEffecting(labels ...string)`       | Mutates external state; needs explicit allowance.  |
| `RequiresApproval()`                    | Blocked unless `PermissionPolicy` allows it.       |
| `Describe(text string)`                 | Schema description shown to the model.             |
| `Param(name, description string)`       | Parameter doc for the generated schema.            |
| `ToolTimeout(value string)`             | Per-tool wall-clock.                               |

### SubAgent options

| Option                  | Purpose                                                       |
| ----------------------- | ------------------------------------------------------------- |
| `MaxParallel(limit int)`| Bound concurrent invocations of the SubAgent.                 |
| `PartialOK()`           | Return ordered error results instead of failing the whole parent. |

### Backoff

| Function              | Purpose                                  |
| --------------------- | ---------------------------------------- |
| `ExponentialBackoff()`| Default exponential retry delay policy.  |

## Outputs

```go
ovr.Reply(format ReplyFormat) Node   // synchronous reply to the trigger
ovr.Push(target PushTarget) Node     // asynchronous outbound to webhook/queue
ovr.Sink(target SinkTarget) Node     // terminate without reply (log/file)
```

| Function                  | Purpose                                            |
| ------------------------- | -------------------------------------------------- |
| `JSON[T]() JSONReply[T]`  | Typed JSON reply with strict schema validation.    |
| `SSE() SSEReply`          | Server-Sent Events streaming reply.                |
| `Accepted() AcceptedReply`| HTTP 202 reply while the pipeline runs async.      |
| `Queue(uri string)`       | Push target publishing to a queue URI (`http(s)://`, `nats://`, `kafka://`, `redis://`, `sqs://`). |
| `Log() LogSink`           | Sink writing to logs.                              |
| `File(path string)`       | Sink writing the result to a file.                 |

`Webhook(url string)` is also valid as a `Push` target for HTTP webhooks (the
trigger constructor reuses the same name for inbound webhooks; context
disambiguates).

## Composition

```go
ovr.Parallel(branches ...Node)
ovr.Map(options ...MapOption, body Node)
```

| Option / Helper           | Purpose                                            |
| ------------------------- | -------------------------------------------------- |
| `Concurrency(limit int)`  | Bound parallel item execution inside `Map`.        |
| `PartialOK()`             | Allow partial outcomes in `Parallel`, `Map`, `SubAgent`. |

`Pipeline(nodes ...Node) PipelineSpec` constructs a child pipeline for
`SubAgent`.

## Capabilities & Advanced

### Bash

```go
ovr.Bash(sandbox ovr.SandboxConfig, options ...ovr.BashOption) ovr.PipeOption
```

| Option                          | Purpose                                            |
| ------------------------------- | -------------------------------------------------- |
| `BashTimeout(value string)`     | Per-command wall-clock.                            |
| `BashMaxOutputBytes(max int)`   | Bound captured stdout/stderr.                      |
| `UnsafeBashHostExecution()`     | Allow host-shell fallback (no OS sandbox).         |

### Sandbox

```go
ovr.Sandbox(root string, options ...ovr.SandboxOption) ovr.SandboxConfig
```

| Option                    | Purpose                                            |
| ------------------------- | -------------------------------------------------- |
| `AllowEnv(keys ...string)`| Allow named env vars inside the sandbox.           |

### Env

```go
ovr.RequireEnv(names ...string) error
```

Fails fast at boot if any named env var is missing.

## Runner

```go
runner := ovr.NewRunner(opts ...ovr.RunnerOption)
runner.Run(addr string, nodes ...ovr.Node) error
ovr.Run(addr string, nodes ...ovr.Node) error      // convenience for the default runner
ovr.Validate(nodes ...ovr.Node) error              // validation without serving
```

| Option                                  | Purpose                                            |
| --------------------------------------- | -------------------------------------------------- |
| `WithPermissionPolicy(p PermissionPolicy)` | Install production permission policy.            |
| `WithStateStore(s StateStore)`          | Custom durable state store.                        |
| `WithHooks(h *Hooks)`                   | Advanced lifecycle hooks.                          |
| `WithSandbox(c SandboxConfig)`          | Filesystem workspace boundary.                     |
| `WithSchemaRepairAttempts(n int)`       | Bounded ResultSchema repair attempts.              |
| `WithTracer(t Tracer)`                  | OTel-compatible span emission.                     |
| `WithOTLPExporter(endpoint string, opts ...OTLPExporterOption)` | Native OTLP/HTTP span export (no otel SDK). |
| `WithPricing(t PricingTable)`           | Cost accounting from a per-model pricing table.    |
| `WithProviderBudget(provider string, maxInFlight int)` | Bound concurrent in-flight LLM calls per provider. |

### Persistent memory

The durable `StateStore` carries scoped agent memory that survives across
sessions. A scope identifies the worker plus logical agent so concurrent agents
stay isolated while one logical agent's memory persists.

```go
type MemoryRecord struct {
    Scope     string
    Key       string
    Value     string
    UpdatedAt time.Time
}

// StateStore memory methods (in addition to execution/session/event methods):
SaveMemory(ctx context.Context, scope, key, value string) error
Memory(ctx context.Context, scope, key string) (string, bool, error)
ListMemory(ctx context.Context, scope string) ([]MemoryRecord, error)
```

Values are bounded in size and redacted before persistence, so secrets and skill
bodies never reach durable storage. Writes are last-write-wins per `(scope, key)`;
entries are not auto-expired — prune by overwriting keys or scoping them with a
generation marker.

## Pricing

```go
type PricingTable map[string]ModelRate   // keyed by "provider/model"
type ModelRate struct {
    InputUSDPerToken      float64
    OutputUSDPerToken     float64
    CacheReadUSDPerToken  float64
    CacheWriteUSDPerToken float64
}
ovr.PerMillion(input, output, cacheRead, cacheWrite float64) ovr.ModelRate
```

When a rate exists for the request model, the harness computes `Usage.CostUSD`
per call (including cache read/write tokens) and aggregates total cost per
execution, surfaced as `cost_usd` on `llm_call_completed` events and in
`/admin/status` and trace detail. When no table is configured (or no rate
matches a model), cost stays best-effort (zero) with no behavior change.

## Tracing

```go
type Tracer interface {
    StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, Span)
}
type Span interface {
    End()
    SetAttribute(key string, value any)
    RecordError(err error)
}
ovr.NopTracer() Tracer
```

The harness emits one span per pipeline, pipe, session, LLM call, tool call,
schema validation, and subagent task. `*_started` events are paired with their
`*_completed` or `*_failed` counterparts internally.

### Native OTLP exporter

`WithOTLPExporter` installs a built-in `Tracer` that ships spans to an
OTLP/HTTP collector using the JSON encoding — no OpenTelemetry SDK dependency.
Default off: when unset, behavior is unchanged.

```go
type OTLPExporterOption // configures the exporter
ovr.OTLPServiceName(name string) OTLPExporterOption
ovr.OTLPHeaders(headers map[string]string) OTLPExporterOption

runner := ovr.NewRunner(
    ovr.WithOTLPExporter("https://collector:4318",
        ovr.OTLPServiceName("orders-api"),
        ovr.OTLPHeaders(map[string]string{"Authorization": "Bearer <token>"}),
    ),
)
```

The exporter appends `/v1/traces` to the endpoint, posts one span per logical
operation, and redacts span attributes (sensitive keys, credential-looking
strings) before export. Export errors are swallowed so observability never
breaks the pipeline. `WithOTLPExporter` is a convenience over `WithTracer`;
when both are passed, the last option wins.

## Hook Events

`EventKind` constants exposed for hooks include:

```go
ovr.EventPipelineStarted
ovr.EventPipelineCompleted
ovr.EventPipelineFailed
ovr.EventPipeStarted
ovr.EventPipeCompleted
ovr.EventPipeFailed
ovr.EventSessionStarted
ovr.EventSessionSaved
ovr.EventSessionCancelled
ovr.EventLLMCallStarted
ovr.EventLLMCallCompleted
ovr.EventLLMCallFailed
ovr.EventLLMTokenDelta
ovr.EventModelFallback
ovr.EventToolCallStarted
ovr.EventToolCallCompleted
ovr.EventToolCallFailed
ovr.EventPermissionDecision
ovr.EventIdempotencyDecision
ovr.EventSignatureDecision
ovr.EventApprovalRequested
ovr.EventApprovalApproved
ovr.EventApprovalDenied
ovr.EventExecutionSuspended
ovr.EventExecutionResumed
ovr.EventHookFailed
ovr.EventSchemaValidationPassed
ovr.EventSchemaValidationFailed
ovr.EventSchemaRepairStarted
ovr.EventSchemaRepairCompleted
ovr.EventSchemaRepairFailed
ovr.EventBudgetExceeded
ovr.EventTaskStarted
ovr.EventTaskCompleted
ovr.EventTaskFailed
ovr.EventSkillLoaded
ovr.EventStreamDeadLettered
ovr.EventStreamRedelivered
ovr.EventCronLeaseAcquired
ovr.EventCronLeaseLost
ovr.EventCronTickSkipped
ovr.EventSinkLogged
```

## Metrics

```
GET /metrics
```

A hand-rolled Prometheus text exposition (no dependency) derived from the
EventStream/StateStore: per-kind counters (`ouvrier_pipeline_*_total`,
`ouvrier_pipe_*_total`, `ouvrier_llm_call_*_total`, `ouvrier_tool_call_*_total`,
`ouvrier_stream_dead_lettered_total`, `ouvrier_stream_redelivered_total`)
plus latency summaries (`ouvrier_llm_call_duration_ms`,
`ouvrier_pipeline_duration_ms`, `ouvrier_pipe_duration_ms`,
`ouvrier_tool_call_duration_ms`) as `_sum`/`_count` series. The endpoint shares
the admin auth posture: bearer-token protected outside dev mode.

## Admin

Bearer-authenticated HTTP endpoints exposed by the runtime:

```
GET  /admin/health
GET  /admin/status
GET  /admin/plans          # compiled trigger/step/terminal capabilities
GET  /admin/capabilities   # integration-oriented alias for compiled capabilities
GET  /admin/events         # redacted JSONL/SSE event stream
GET  /admin/traces?last=N
GET  /admin/traces/<exec-id>
POST /admin/trigger        # returns exec_id/trace_id/session_id when scheduled
GET  /admin/approvals      # pending human-in-the-loop approvals
POST /admin/approvals/<id> # approve or deny a suspended tool call
POST /admin/streams/replay # replay the runtime-retained DLQ copy for a stream plan
GET  /metrics              # Prometheus text exposition (admin token required)
GET  /dev                  # dev-mode trace viewer (admin token required)
```

Use the `ouvrier status` / `ouvrier logs` / `ouvrier trace` CLI to consume
them from the terminal.

## Permission Policy

```go
ovr.AllowSideEffects(labels ...string) ovr.PermissionPolicy
ovr.AllowSideEffectTargets(label string, targets ...string) ovr.PermissionPolicy
```

Use `AllowSideEffects` for non-targeted Go tool labels. Use
`AllowSideEffectTargets` for target-scoped output actions such as webhook push,
queue push, file sink, MCP, Bash process, and Bash filesystem access. Compose
more sophisticated policies by implementing the `PermissionPolicy` interface.
`PermissionDecision` exposes `Allowed`, `Reason`, `Suspended`, and `ApprovalID`;
a suspended decision must carry a non-empty approval ID so the runtime can
resume after an operator approves it.

## Stability

This document is the current shipped public reference. Anything not listed here
is considered internal and may change between patch releases. Spec gaps
discovered by `api_parity_test.go` are logged at the top of that file.
