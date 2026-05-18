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
- Final module path is still to be decided. Do not ship with
  `github.com/yourorg/ouvrier`.

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

### M0 - Product And API Groundwork

- Fix contradictions between `specs.md` and the PDF.
- Decide final Go module path.
- Define public contracts for `Node`, `From`, `Pipe`, `Run`, `Reply`, `Push`,
  `Sink`, `Parallel`, `Map`, and `SubAgent`.
- Define retry, side-effect, idempotency, and error semantics.
- Define model ID policy and provider naming.

### M1 - Runtime Core

- Implement pipeline validation.
- Implement linear pipeline execution.
- Implement HTTP triggers: `GET`, `POST`, path params, body parsing.
- Implement cron trigger.
- Implement `Run(addr string, nodes ...Node) error`.
- Implement context propagation, timeouts, graceful shutdown.

### M2 - Agent Harness

- Implement Anthropic Messages provider.
- Implement tool-use loop.
- Implement Go tool registration and reflection schema generation.
- Implement `Output[T]()` and JSON schema validation.
- Implement budgets: max iterations, tokens, cost, wallclock.
- Implement retry/backoff and transient/permanent error classification.
- Implement Anthropic prompt caching with `cache_control`.

### M3 - Capabilities

- Implement `Tool`, `Describe`, and `Param`.
- Implement `Skill` loading, frontmatter validation, and system prompt injection.
- Generate user-project embed code for skills and support files.
- Implement MCP client support.
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
- Integration tests for CLI scaffold/build/dev.
- Mock-provider tests for harness behavior.
- Security tests for admin auth, webhook signatures, and secret redaction.
- `gofmt` required.
- `go vet` required.
- `staticcheck` required.
- v0.1 is complete only when all documented criteria in `specs.md` pass.
