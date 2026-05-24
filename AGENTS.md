# Ouvrier Project Instructions

## Mission

Build Ouvrier v0.1 as specified in `specs.md` and `ouvrier-doc-v0.1.pdf`.

The v0.1 scope is non-negotiable: all documented runtime, CLI, harness,
observability, security, deployment, and documentation capabilities must ship.
Proceed incrementally, but do not remove scope from v0.1.

## Environment

- Required Go version: Go 1.25 or newer.
- Current verified version: `go version go1.25.6 linux/amd64`.
- Language: Go.
- Runtime package: `ovr`.
- CLI binary: `ouvrier`.
- Public Go module path is `github.com/ArnaudGuiovanna/ouvrier`. Scaffolded
  projects, examples, and documentation must import the framework under that
  path. A local `replace` directive is used during pre-publish development.

## Primary Sources

- `specs.md`: implementation specification.
- `ouvrier-doc-v0.1.pdf`: user-facing documentation and examples.

If these disagree, treat it as a product/spec bug to resolve explicitly. Do not
silently pick one behavior when it affects public API, security, or generated
user code.

## Technical Principles

- Prefer small vertical slices that compile and can be demonstrated.
- Keep public API minimal, explicit, and Go-idiomatic.
- Every public primitive must have validation and tests before being treated as
  complete.
- Use TDD whenever it is practical: start with focused tests for public behavior,
  implement the smallest code that passes, then refactor.
- Keep files small and cohesive. Prefer files under 300 lines, split before 500
  lines, and document any deliberate exception.
- Factor by responsibility, not by premature abstraction. A coding agent and a
  human maintainer must be able to locate behavior quickly.
- Prefer narrow packages with clear ownership over large catch-all packages.
- Avoid hidden global state. Use explicit dependencies and small interfaces at
  boundaries that need substitution in tests.
- Public behavior must be covered by tests close to the package that owns it.
- Side-effecting tools must be designed with retry and idempotency in mind.
- User-generated projects must remain readable Go. The CLI may edit files, but
  it must not create opaque magic.
- Secrets must never be embedded into binaries or logged.

## SOTA Harness Invariant

Ouvrier v0.1 must ship a SOTA agent harness. This is non-negotiable for the
current version, not a roadmap item.

The public mental model must remain simple: trigger, goal, tools, outcome.
The agent and its harness are a convenience provided by Ouvrier, not complexity
that every user must manage.

The default syntax must stay close to:

```go
ovr.Run(":8080",
    ovr.From("POST /tickets"),
    ovr.Pipe("Triage le ticket",
        ovr.Model("anthropic/claude-sonnet-4-6"),
        ovr.Tool("load_ticket", LoadTicket),
        ovr.Output[Triage](),
    ),
    ovr.Reply(ovr.JSON[Triage]()),
)
```

Users should not have to instantiate or understand the ten harness components
for normal use. Every Pipe must still execute through a production-grade
internal harness with these ten components:

1. `Harness` - the coordinator for one Pipe execution.
2. `Session` - per-execution state, messages, lineage, budgets, trace IDs, and
   cancellation.
3. `ToolExecutor` - the only path for running Go tools, MCP tools, Bash,
   sandbox file operations, and SubAgent tasks.
4. `Sandbox` - isolated workspace/process/env/network boundary for risky
   capabilities, with fail-fast if requested guarantees cannot be enforced.
5. `PermissionPolicy` - deterministic authorization for filesystem, env,
   network, process, side effects, MCP, and SubAgent calls.
6. `HookBus` - internal extension points around prompts, LLM calls, tool calls,
   schema validation, budgets, and subagent completion.
7. `EventStream` - append-only event source for traces, logs, SSE, admin
   endpoints, and the dev trace viewer.
8. `StateStore` - execution/session history, idempotency, traces, and schema
   violations. Memory remains the lightweight test backend. The durable v0.1
   default is embedded SQLite through `modernc.org/sqlite`, behind the same
   `Store` interface. PostgreSQL may be added later without changing harness
   contracts.
9. `ResultSchema` - JSON Schema generation and strict validation for
   `Output[T]()` and typed replies, with observable repair attempts when
   enabled.
10. `SubAgent/Task` - governed child pipeline execution with child sessions,
    inherited budgets, cancellation propagation, `MaxParallel`, and ordered
    outcomes.

No tool call may bypass `ToolExecutor`. No privileged action may bypass
`PermissionPolicy`. No user-visible trace/log/admin output may bypass secret
redaction. No v0.1 acceptance should treat a fake tool-result loop as complete.

Advanced users may opt into selected public configuration surfaces such as
`NewRunner`, `WithStateStore`, `WithPermissionPolicy`, `WithHooks`, `Sandbox`,
`ReadOnly`, `SideEffecting`, `Idempotent`, `SubAgent`, `Pipeline`, and
`MaxParallel`. The concrete `Harness`, `Session`, `ToolExecutor`,
`EventStream`, and `ResultSchema` implementations should remain internal unless
there is a clear product reason to expose them later.

## TUI Requirement

The `ouvrier` CLI TUI must use Charm Bracelet Bubble Tea.

- Bubble Tea source: `https://github.com/charmbracelet/bubbletea`.
- Use the current v2 import style unless implementation constraints require a
  documented exception: `tea "charm.land/bubbletea/v2"`.
- Use Charm Bracelet ecosystem components where appropriate:
  - Bubble Tea for state/update/view architecture.
  - Bubbles for standard input/list/text-area components.
  - Lip Gloss for styling.
- TUI style must follow the spec: black, off-white, phosphor green `#00d27a`,
  monospace, restrained boxes, no decorative animation.

## v0.1 Backlog

GitHub Issues is the official operational backlog for v0.1. Use milestone
`v0.1` in `ArnaudGuiovanna/ouvrier` as the source of truth for work tracking,
priorities, and closure state.

The sections below remain implementation context and historical planning
material derived from `specs.md` and the PDF. When the Markdown backlog and
GitHub Issues disagree, update GitHub Issues first and treat the mismatch as a
backlog hygiene task rather than running two separate backlogs.

### Current Implementation Snapshot

Status date: 2026-05-18.

Estimated v0.1 completion: **38%**.

This is a pragmatic estimate, not a release metric. The core runtime/harness
foundation is now usable for narrow HTTP + LLM + Go/MCP tool slices, but v0.1
is not yet production-complete because schema enforcement, retries,
observability/admin, SubAgent/Task, deployment, streams, and full CLI workflows
are still incomplete.

| Area | Estimate | Current state |
| --- | ---: | --- |
| M0 Product/API groundwork | 70% | Public primitives and harness direction are mostly settled; `Parallel`, `Map`, `SubAgent`, final module path, and retry semantics still need final API hardening. |
| M1 Runtime core | 60% | Compiled plans, validation, HTTP routes, graceful shutdown, async accepted replies, and worker pools exist. Cron runtime, path-param extraction, timeouts, and non-HTTP transports remain. |
| M2 SOTA agent harness | 45% | Session, EventStream package, StateStore memory/SQLite, PermissionPolicy, Sandbox, ToolExecutor, providers, and basic tool loop exist. HookBus, strict ResultSchema validation/repair, full budget accounting, retry/backoff, and SubAgent/Task remain. |
| M3 Capabilities | 55% | Go tools, permission metadata, skill declaration, and official MCP SDK integration exist. Skill loading/runtime prompt injection and Bash sandbox remain incomplete. |
| M4 Composition/concurrency | 15% | Trigger `WorkerPool` exists. `Parallel`, `Map`, `PartialOK`, `SubAgent`, and `MaxParallel` remain. |
| M5 Outputs | 30% | `Reply(JSON[T]())`, `Reply(Accepted())`, `Push`, and `Sink` compile. SSE and real outbound push/queue/file/log sinks remain. |
| M6 Observability/admin | 10% | Event and state foundations exist. Admin endpoints, trace viewer, spans, redaction, metrics, and OTel export remain. |
| M7 CLI/TUI | 15% | Minimal CLI/TUI/scaffold exists with Bubble Tea. Most `add`, `show`, `dev`, `build`, `status`, `logs`, and `trace` workflows remain. |
| M8 Deployment | 8% | Scaffold contains deploy config shape. Real static build, SSH deploy, Docker deploy, rollback, and health checks remain. |
| M9 Streams/webhooks | 8% | Webhook/stream declarations exist. HMAC verification, runtime stream consumers, idempotent trigger handling, backpressure, DLQ, Kafka/NATS/Redis remain. |
| M10 Examples/docs | 10% | Specs/backlog are maintained. Reference examples, README, API reference, and PDF corrections remain. |
| M11 Quality gates | 35% | Unit tests are broad and `go test ./...` / `go vet ./...` pass. Race tests, staticcheck, security tests, golden tests, and integration/dev/deploy tests remain. |

Recommended next strict order:

1. Finish `ResultSchema`: real JSON Schema generation, strict output
   validation, observable schema violation persistence, and bounded repair.
2. Wire `EventStream` and `HookBus` into the harness path; no trace/admin view
   should read from ad hoc state.
3. Implement retry/backoff and idempotency behavior without duplicating side
   effects.
4. Implement full budget enforcement: iterations, tokens, cost, wallclock, and
   child budgets.
5. Implement governed `SubAgent/Task` with child sessions, inherited policies,
   `MaxParallel`, cancellation, and ordered outcomes.
6. Add admin/health/status/traces on top of `EventStream` + `StateStore`.
7. Then continue CLI depth, deployment, streams/webhooks, examples, and docs.

### Immediate Implementation Order

The next implementation work must follow this order. Do not advance CLI,
deployment, streams, or broad integrations ahead of these foundations.

1. Done: build `internal/runtime` plan compilation: `Plan`, `Trigger`,
   `Step`, `Terminal`, strict validation, and the multi-pipeline strategy for
   `Run`.
2. Done: tighten public pipeline validation: terminal must be unique and last,
   trigger/output compatibility must be checked, and no node may be ignored.
3. Partial: implement `Output[T]()` and the first `ResultSchema` contract.
   Type capture exists; strict JSON Schema generation, runtime validation,
   violation recording, and repair remain.
4. Done: introduce structured `Session`.
5. Partial: introduce append-only `EventStream`. The package exists and is
   tested; harness/admin integration remains.
6. Done: introduce `StateStore` abstraction with memory and embedded SQLite
   backends.
7. Done: implement real Go-only `ToolExecutor`.
8. Done: prove the loop with a mock provider that calls a real Go tool through
   `ToolExecutor`.
9. Done: connect `runtime_http` to the compiled runtime/harness path so a
   simple HTTP Pipe no longer returns a fake `501` for implemented cases.
10. In progress: primary providers, sandbox hardening, official MCP SDK,
    async replies, worker pools, and SQLite StateStore have started. Next
    priority is ResultSchema + EventStream/HookBus wiring before admin,
    SubAgent/Task, CLI depth, deployment, and streams.

Each step must be TDD-backed and leave `go test ./...` and `go vet ./...`
passing before commit.

### M0 - Product And API Groundwork

- Fix contradictions between `specs.md` and the PDF.
- Done: final Go module path is `github.com/ArnaudGuiovanna/ouvrier`.
- Define public contracts for `Node`, `From`, `Pipe`, `Run`, `Reply`, `Push`,
  `Sink`, `Parallel`, `Map`, and `SubAgent`.
- Define the SOTA harness contracts and internal package boundaries:
  `internal/runtime`, `internal/harness`, `internal/tools`,
  `internal/sandbox`, `internal/policy`, `internal/events`, `internal/state`,
  `internal/schema`, and `internal/provider`.
- Define retry, side-effect, idempotency, and error semantics.
- Define model ID policy and provider naming.

### M1 - Runtime Core

- Implement pipeline validation.
- Implement linear pipeline execution.
- Implement HTTP triggers: `GET`, `POST`, path params, body parsing.
- Implement cron trigger.
- Implement `Run(addr string, nodes ...Node) error`.
- Implement context propagation, timeouts, graceful shutdown.

### M2 - SOTA Agent Harness

- Implement structured `Session` and child-session lineage.
- Implement append-only `EventStream` and synchronous `HookBus`.
- Implement `StateStore` backends for executions, sessions, traces,
  idempotency keys, and schema violations: memory for tests and embedded
  SQLite as the durable single-binary default.
- Implement `ResultSchema`, `Output[T]()` and strict JSON validation.
- Implement `PermissionPolicy` with secure defaults and auditable decisions.
- Implement `Sandbox` abstraction for filesystem, env, process, and network
  boundaries.
- Implement `ToolExecutor` for Go tools, MCP tools, Bash tools, sandbox file
  tools, and SubAgent tools.
- Implement primary providers with tool use, provider metadata, error
  classification, and usage/cost accounting where the upstream API allows it:
  Anthropic, OpenAI, Ollama, Mistral, Gemini, and vLLM.
- Implement the real tool-use loop with budgets: max iterations, tokens, cost,
  wallclock, and child-task budgets.
- Implement retry/backoff without duplicating side effects: retry only
  read-only/idempotent tool work or provider calls before side effects.
- Implement `SubAgent/Task` execution with `MaxParallel`, inherited budgets,
  cancellation, and ordered outcomes.

### M3 - Capabilities

- Implement `Tool`, `Describe`, and `Param`.
- Implement `Skill` loading, frontmatter validation, and system prompt injection.
- Generate user-project embed code for skills and support files.
- Implement MCP client support with the official Go SDK:
  `github.com/modelcontextprotocol/go-sdk/mcp`. Do not implement a protocol
  client by hand unless the SDK blocks a required v0.1 capability and the
  exception is documented.
- Implement Bash sandbox with explicit platform limitations.

### M4 - Composition And Concurrency

- Implement `Parallel` with ordered outcomes and fail-fast.
- Implement `PartialOK`.
- Implement `Map` with bounded `Concurrency`.
- Implement `WorkerPool` for triggers.
- Implement `SubAgent` and `MaxParallel`.

### M5 - Outputs

- Implement `Reply(JSON[T]())`.
- Implement `Reply(SSE())`.
- Implement `Reply(Accepted())`.
- Implement `Push(Webhook(url))`.
- Implement `Push(Queue(url))`.
- Implement `Sink(Log())`.
- Implement `Sink(File(path))`.

### M6 - Observability And Admin

- Implement execution history.
- Implement spans for pipeline, pipe, LLM call, and tool call.
- Implement OTel export hooks.
- Implement functional health metrics.
- Implement schema conformance metrics.
- Implement protected admin endpoints:
  - `GET /admin/health`
  - `GET /admin/status`
  - `GET /admin/traces?last=N`
  - `GET /admin/traces/<exec-id>`
  - `POST /admin/trigger`
- Define and implement dev-mode auth behavior without contradicting production
  security.

### M7 - CLI And TUI

- Implement `ouvrier new` with Bubble Tea TUI.
- Implement `ouvrier add agent`.
- Implement `ouvrier add tool`.
- Implement `ouvrier add skill`.
- Implement `ouvrier show`.
- Implement `ouvrier dev` with hot reload and trace viewer.
- Implement `ouvrier build`.
- Implement `ouvrier status`.
- Implement `ouvrier logs`.
- Implement `ouvrier trace`.
- Keep CLI screens consistent with the documented visual identity.

### M8 - Deployment

- Implement static build path with `CGO_ENABLED=0`.
- Implement SSH deploy: build, upload, `.env` chmod 0600, systemd unit,
  restart, healthcheck, rollback.
- Implement Docker deploy: distroless Dockerfile, build, tag, optional push.
- Implement `.env`, `.env.example`, `.gitignore`, and `pip.yaml` generation.

### M9 - Streams And Webhooks

- Implement signed webhook verification with HMAC-SHA256.
- Implement idempotency key handling.
- Implement Kafka stream support.
- Implement NATS stream/queue support.
- Implement Redis stream support.
- Define and implement ack, retry, backpressure, and DLQ behavior.

### M10 - Examples And Documentation

- Implement Moodle FSRS reference example.
- Implement ticket triage reference example.
- Correct the PDF documentation mismatches.
- Provide a junior-friendly README.
- Provide complete API reference.
- Document limitations without contradicting promised behavior.

### M11 - Quality Gates

- Unit tests for runtime.
- Unit tests for every SOTA harness component.
- Integration tests for CLI scaffold/build/dev.
- Mock-provider tests for harness behavior.
- Security tests for admin auth, webhook signatures, and secret redaction.
- Security tests for sandbox escape, permission deny, env leakage, and retry
  without duplicate side effects.
- Race tests for runtime, state store, event stream, tool executor, and
  subagent/task concurrency.
- Golden tests for examples and docs/API drift.
- `gofmt` required.
- `go vet` required.
- `staticcheck` required.
- v0.1 is complete only when all documented criteria in `specs.md` pass.
