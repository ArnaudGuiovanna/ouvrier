# Ouvrier

Workers for your APIs.

Ouvrier is a Go framework for building small agentic middleware services. You
declare a trigger, one or more LLM-powered pipes, the tools they can use, and
the outcome. Ouvrier turns that declaration into a service with typed results,
tool governance, traces, durable state, and deployment-oriented defaults.

```go
package main

import (
	"context"
	"log"

	ovr "github.com/ArnaudGuiovanna/ouvrier"
)

type Ticket struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type Triage struct {
	Priority string `json:"priority"`
	Summary  string `json:"summary"`
}

func LoadTicket(ctx context.Context, args struct {
	ID string `json:"id"`
}) (Ticket, error) {
	return Ticket{ID: args.ID, Subject: "Login issue", Body: "User cannot sign in."}, nil
}

func main() {
	if err := ovr.Run(":8080",
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Tool("load_ticket", LoadTicket,
				ovr.ReadOnly(),
				ovr.Describe("Load one support ticket by ID."),
				ovr.Param("id", "Ticket identifier from the request path."),
			),
			ovr.Output[Triage](),
		),
		ovr.Reply(ovr.JSON[Triage]()),
	); err != nil {
		log.Fatal(err)
	}
}
```

## Status

Ouvrier v0.1 is feature-complete against the documented milestone backlog.
The public Go module path is:

```txt
module github.com/ArnaudGuiovanna/ouvrier
```

Generated projects import the framework under that path. During local
development before the repository is published, scaffolded projects add a
`replace github.com/ArnaudGuiovanna/ouvrier => <local-checkout>` directive so
they keep building against the working tree.

The CI gate (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`,
`staticcheck`, `go test ./...`, race tests on concurrency-sensitive
packages, and builds the `ouvrier` CLI on every push and pull request.

What ships in v0.1:

- HTTP, Cron, Webhook, and Stream (NATS / Redis / Kafka) triggers with
  worker pools, signed inbound HMAC verification, and idempotency keys.
- Sequential, `Parallel`, `Map`, `Concurrency`, and `PartialOK` composition.
- The full SOTA harness: `Session`, `EventStream`, `HookBus`, `StateStore`
  (memory + embedded SQLite), `PermissionPolicy`, `Sandbox`, `ToolExecutor`,
  `ResultSchema`, governed `SubAgent`/Task.
- Go tools, MCP through the official Go SDK, Skill loading with
  frontmatter validation, and a sandboxed Bash capability with workspace,
  env allowlist, timeout, output bounds, and process cleanup.
- Provider adapters for `anthropic/*`, `openai/*`, `ollama/*`, `mistral/*`,
  `gemini/*`, `vllm/*`, `groq/*`, `deepseek/*`, `azure/*`, and `bedrock/*`
  with tool use, typed final results, and classified errors.
- Reply (`JSON[T]`, `SSE`, `Accepted`), Push (webhook, queue), and Sink
  (log, file) outputs.
- Protected admin endpoints (`/admin/health`, `/admin/status`,
  `/admin/traces`, `/admin/traces/<id>`, `/admin/trigger`), a Prometheus
  `/metrics` endpoint, plus a dev-mode trace viewer at `/dev`.
- OpenTelemetry-compatible `Tracer` hook (and a built-in native OTLP/HTTP
  span exporter via `WithOTLPExporter`) for pipeline, pipe, session, LLM,
  tool, schema, and subagent spans.
- The `ouvrier` CLI: `version`, `new`, `show`, `status`, `logs`, `trace`,
  `add agent|tool|skill`, `dev`, `build` (static cross-compile), and
  `deploy ssh|docker`.

Operational notes for v0.1: provider response streaming is not surfaced
through `EventStream`, `ouvrier dev` runs `go run .` without hot reload, and
provider cost estimates are best-effort from available provider metadata.

## Mental Model

An Ouvrier worker has four parts:

```txt
From -> Pipe -> Pipe -> Reply / Push / Sink
```

- `From` receives work from HTTP, cron, webhooks, or streams.
- `Pipe` runs one LLM agent step with a goal, model, tools, skills, MCP, and
  typed output.
- `Tool` exposes Go functions to the agent through a single governed executor.
- `Reply`, `Push`, and `Sink` decide what happens to the final outcome.

The public model stays small: trigger, goal, tools, outcome. Internally, every
`Pipe` runs through the Ouvrier harness: session, tool executor, sandbox,
permission policy, hook bus, event stream, state store, result schema, and
subagent task execution.

## Requirements

- Go 1.25 or newer.
- A provider credential for hosted models, for example `ANTHROPIC_API_KEY`.
- Optional local model runtime for `ollama/*`.

The repository is currently verified with:

```sh
go version go1.25.6 linux/amd64
```

## Install From Source

Clone the repository and build the CLI from the checkout:

```sh
git clone git@github.com:ArnaudGuiovanna/ouvrier.git
cd ouvrier
go test ./...
go install ./cmd/ouvrier
ouvrier version
```

During local development, generated projects use a `replace` directive pointing
back to this checkout.

## Create A Worker

The non-interactive scaffold is the fastest way to create a development worker.
It generates compiling HTTP, cron, webhook, and stream workers:

```sh
ouvrier new \
  --yes \
  --name ticket-triage \
  --trigger "POST /tickets/{id}" \
  --model "anthropic/claude-sonnet-4-6" \
  --dir /tmp
```

The `--trigger` flag accepts every supported trigger category. HTTP workers
reply with JSON; cron, webhook, and stream workers terminate with
`Sink(Log())`:

```sh
--trigger "POST /tickets"             # http  -> Reply(JSON)
--trigger "0 6 * * *"                 # cron  -> Sink(Log())  (or "cron 0 6 * * *")
--trigger "webhook github"            # webhook -> Sink(Log())
--trigger "stream kafka://tickets"    # stream  -> Sink(Log())
```

Then run it:

```sh
cd /tmp/ticket-triage
export ANTHROPIC_API_KEY=...
go run .
```

Call the worker:

```sh
curl -X POST http://localhost:8080/tickets/T-123 \
  -H 'Content-Type: application/json' \
  -d '{"message":"user cannot sign in"}'
```

## Core API

### Triggers

```go
ovr.From("POST /tickets/{id}")
ovr.From("GET /health")
ovr.From(ovr.Cron("0 6 * * *"))
ovr.From(ovr.Webhook("github"))
ovr.From(ovr.Stream("kafka://tickets"))
```

HTTP triggers are the primary server runtime. Cron plans with `Push` or `Sink`
terminals now execute through the same harness path when run as a cron-only
runtime. Webhook trigger plans now expose `POST /webhooks/<provider>` routes,
wrap the request as `{trigger, provider, body}`, and reuse the same signature,
idempotency, harness, state, event, and policy guarantees as HTTP triggers.
Mixed HTTP, Webhook, Cron, and Stream runtimes can run together. Stream
receivers support NATS, Redis Streams, and Kafka boundaries, reserve message
IDs in the state store when available, and emit stream dead-letter events for
failed delivery handling.

Stream production hardening:

- `StreamDLQ(target, maxAttempts)` retries a poisoned message up to
  `maxAttempts` deliveries, then routes it to the configured dead-letter target.
  The target is published to the real broker transport (`kafka://`, `nats://`,
  `redis://`) through the same producer machinery as the `Queue` push terminal.
- `StreamMaxInFlight(limit)` bounds concurrently processed deliveries so a slow
  handler applies backpressure to the broker.
- `StreamAckPolicy(StreamAckAuto | StreamAckManual)` selects the per-broker
  acknowledgement mode. `StreamAckManual` leaves acking to the handler; brokers
  whose receiver exposes no ack closure treat it as a no-op.
- Replay a dead-letter queue with `ReplayStreamDLQ` in-process, or via the
  admin endpoint `POST /admin/streams/replay` (same admin auth as other
  `/admin/*` routes), which drains and reprocesses the DLQ and returns the
  replayed count. Dead-letter targets are credential-stripped in events/logs.

### Pipes

```go
ovr.Pipe("Summarize the request.",
	ovr.Model("openai/gpt-5.1"),
	ovr.Output[Summary](),
)
```

Every `Pipe` must declare an explicit model. Model IDs use a provider prefix:

- `anthropic/*`
- `openai/*`
- `ollama/*`
- `mistral/*`
- `gemini/*`
- `vllm/*`
- `groq/*`
- `deepseek/*`
- `azure/*` (Azure OpenAI deployments)
- `bedrock/*` (AWS Bedrock Converse models)

### Tools

Tools are normal Go functions. The first parameter must be `context.Context`,
and the return shape must be `error` or `(value, error)`.

```go
func LookupCustomer(ctx context.Context, args struct {
	Email string `json:"email"`
}) (Customer, error) {
	// Query your own system here.
}

ovr.Tool("lookup_customer", LookupCustomer,
	ovr.ReadOnly(),
	ovr.Describe("Find a customer by email address."),
	ovr.Param("email", "Customer email address."),
)
```

Classify tools deliberately:

- `ReadOnly()` for side-effect-free work.
- `Idempotent("key.expression")` for replay-safe side effects.
- `SideEffecting("label")` for mutating work that needs explicit allowance.
- `RequiresApproval()` for actions blocked unless policy allows them.

Ouvrier validates tool arguments against generated JSON Schema before calling
the function. Tool panics become structured tool errors.

### Skills

Reusable Markdown skills live under `skills/<name>/SKILL.md` and can be attached
to a `Pipe`:

```go
ovr.Pipe("Triage the ticket.",
	ovr.Model("anthropic/claude-sonnet-4-6"),
	ovr.Skill("ticket-triage"),
	ovr.Output[Triage](),
)
```

`SKILL.md` must include `name` and `description` frontmatter. Ouvrier loads the
file through the workspace sandbox, injects the Markdown body into the system
prompt in declaration order, and emits `skill_loaded` events without logging the
skill body.

### Typed Results

Use `Output[T]()` to force the final answer of a `Pipe` to match a Go type:

```go
type Summary struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

ovr.Pipe("Summarize the ticket.",
	ovr.Model("anthropic/claude-sonnet-4-6"),
	ovr.NoCache(),
	ovr.Output[Summary](),
)
```

Ouvrier generates JSON Schema from `T`, injects it into the harness prompt, and
validates final JSON strictly. Common model wrappers such as fenced `json`
blocks or short prose around a JSON object/array are stripped before validation;
the schema itself remains strict. Violations are recorded in `EventStream` and
`StateStore`; bounded repair is supported by the harness where enabled.
Static prompt-cache hints are enabled by default; `NoCache()` disables them for
one `Pipe`.

### Outputs

```go
ovr.Reply(ovr.JSON[Summary]())
ovr.Reply(ovr.SSE())
ovr.Reply(ovr.Accepted())
ovr.Push(ovr.Webhook("https://example.com/result"))
ovr.Push(ovr.Queue("nats://127.0.0.1:4222/results"))
ovr.Push(ovr.Queue("kafka://broker:9092/results"))
ovr.Push(ovr.Queue("redis://127.0.0.1:6379/results"))
ovr.Push(ovr.Queue("sqs://sqs.us-east-1.amazonaws.com/123456789012/results"))
ovr.Sink(ovr.Log())
ovr.Sink(ovr.File("./out/result.json"))
```

`Reply(JSON[T]())`, `Reply(SSE())`, `Reply(Accepted())`, webhook push,
HTTP/NATS/Kafka/Redis/SQS queue push, log sink, and file sink have runtime
coverage. Push and file sink terminals run as governed output tools and require
matching permission policy when they perform side effects.

Queue push schemes: `http(s)://` POSTs the body; `nats://` publishes to the
subject; `kafka://host:9092/topic` produces to the topic (via
`segmentio/kafka-go`); `redis://host:6379/stream` appends to the stream with
`XADD` (body stored under the `body` field); `sqs://sqs.<region>.amazonaws.com/<account>/<queue>`
calls SendMessage as a SigV4-signed HTTPS request using the standard `AWS_*`
environment credentials (no aws-sdk dependency). An `idempotency_key` query
parameter is propagated where the protocol supports it: the Kafka message key,
the SQS `MessageDeduplicationId`, and a Redis `idempotency_key` stream field.

### SubAgents

A `SubAgent` exposes a child pipeline as a governed tool to a parent `Pipe`.

```go
translator := ovr.Pipeline(
	ovr.Pipe("Translate the input to French.",
		ovr.Model("anthropic/claude-haiku-4-5"),
		ovr.Output[Translation](),
	),
)

ovr.Pipe("Draft a multilingual reply.",
	ovr.Model("anthropic/claude-sonnet-4-6"),
	ovr.SubAgent("translate", translator, ovr.MaxParallel(2)),
)
```

SubAgents run through `ToolExecutor`, inherit budgets, create child sessions,
propagate cancellation, and attach child events to the parent trace.

### Composition

`Parallel` fans out the same input to several Pipe branches and returns ordered
outcomes. By default, one failed branch cancels sibling work and fails the
composition:

```go
ovr.Parallel(
	ovr.Pipe("Check quality", ovr.Model("anthropic/claude-haiku-4-5")),
	ovr.Pipe("Check compliance", ovr.Model("anthropic/claude-haiku-4-5")),
)
```

`Map` expects the previous step to return a JSON array and runs a Pipe
sub-pipeline for each item with bounded concurrency:

```go
ovr.Map(
	ovr.Concurrency(10),
	ovr.Pipe("Score one item", ovr.Model("anthropic/claude-haiku-4-5")),
)
```

`PartialOK()` changes `Parallel`, `Map`, and `SubAgent` failure behavior to
return ordered partial outcomes instead of failing immediately.

## Configuration

Provider configuration is read from the environment:

```sh
ANTHROPIC_API_KEY=
ANTHROPIC_BASE_URL=
OPENAI_API_KEY=
OPENAI_BASE_URL=
MISTRAL_API_KEY=
MISTRAL_BASE_URL=
GEMINI_API_KEY=
GEMINI_BASE_URL=
OLLAMA_BASE_URL=
VLLM_API_KEY=
VLLM_BASE_URL=
GROQ_API_KEY=
GROQ_BASE_URL=
DEEPSEEK_API_KEY=
DEEPSEEK_BASE_URL=
AZURE_OPENAI_API_KEY=
AZURE_OPENAI_BASE_URL=
AZURE_OPENAI_API_VERSION=
AWS_ACCESS_KEY_ID=
AWS_SECRET_ACCESS_KEY=
AWS_SESSION_TOKEN=
AWS_REGION=
AWS_BEDROCK_BASE_URL=
```

AWS Bedrock requests are authenticated with hand-rolled SigV4 signing (no
aws-sdk dependency); set `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and
`AWS_REGION` to enable the `bedrock/*` prefix. Real-AWS integration testing is
tracked as follow-up; unit coverage exercises the signing helper and the
Converse request/response mapping against a fake HTTP doer.

Runtime/admin configuration:

```sh
PIP_ADMIN_TOKEN=
OUVRIER_STATE_BACKEND=sqlite
OUVRIER_STATE_PATH=.ouvrier/state.db
```

Admin endpoints are mounted under `/admin/*` for HTTP, webhook, cron-only,
stream-only, and mixed runtimes. Set `PIP_ADMIN_TOKEN` outside local
development when exposing a worker.

During local development, `ouvrier dev` auto-loads a `.env` file from the
project directory into the worker's environment. The real process environment
always takes precedence, so any variable you set explicitly is never
overridden, and `.env` values are never printed. This is dev-only and does not
affect deployed/prod binaries; pass `--no-dotenv` to opt out.

## Observability

Ouvrier records structured events for pipeline, pipe, session, LLM, tool,
permission, schema, budget, sink, and subagent task activity.

Runtime endpoints:

```txt
GET  /admin/health
GET  /admin/status
GET  /admin/traces?last=N
GET  /admin/traces/{exec-id}
POST /admin/trigger
GET  /metrics              # Prometheus text exposition (admin token required)
```

Events are redacted before persistence or admin output. The dev trace viewer is
available at `/dev` behind the same admin authorization behavior.

`GET /metrics` renders a hand-rolled Prometheus exposition (counters and latency
summaries derived from the EventStream/StateStore) behind the same admin auth.
For external tracing, `ovr.WithOTLPExporter("https://collector:4318")` ships
spans to an OTLP/HTTP collector with no OpenTelemetry SDK dependency.

## CLI

Current commands:

```sh
ouvrier version
ouvrier new --yes --name NAME --trigger "POST /path" --model "provider/model"
ouvrier new
ouvrier add agent --name NAME --model "provider/model" [--goal TEXT]
ouvrier add tool --name LoadTicket [--describe TEXT] [--readonly|--side-effecting|--idempotent KEY]
ouvrier add skill --name ticket-triage [--description TEXT]
ouvrier show [--dir .]
ouvrier dev [--dir .] [--addr :8080]
ouvrier status [--url http://127.0.0.1:8080] [--token TOKEN]
ouvrier logs   [--url URL] [--token TOKEN] [--last N]
ouvrier trace  <exec-id> [--url URL] [--token TOKEN]
ouvrier build  [--static] [--target os/arch] [--output PATH] [--dir .]
ouvrier deploy ssh --host HOST [--dir .]
ouvrier deploy docker [--dir .] [--image IMAGE] [--tag TAG] [--push]
```

`ouvrier new` opens the Bubble Tea v2 project wizard. The wizard and
`ouvrier new --yes` support HTTP trigger strings such as `"POST /tickets"`.

The introspection commands (`show`, `status`, `logs`, `trace`) read from the
project filesystem (`pip.yaml`) or talk to a running worker through
`/admin/*` using `PIP_ADMIN_TOKEN` for bearer auth. `ouvrier build`
compiles the worker; `--static` implies `CGO_ENABLED=0` with
`-ldflags="-s -w"` and supports `--target os/arch` for cross-compilation
(`modernc.org/sqlite` is pure Go, so static cross-builds work without a C
toolchain).

`ouvrier deploy ssh` ships a static binary, `.env`, `skills/` runtime assets
when present, and a systemd unit with health-check rollback. `ouvrier deploy
docker` renders and builds a distroless container image that includes `skills/`
when present.

## Reference Examples

Two reference workers live under `examples/`:

- `examples/ticket-triage` — the canonical README example with a typed
  `Triage{Priority, Summary, Tags}` reply, an in-memory `load_ticket` Go
  tool, and a `ticket-triage` skill.
- `examples/moodle-fsrs` — a `POST /reviews` worker with a stub
  `compute_fsrs` tool and a typed `Decision{NextDue, Stability, Difficulty,
  Lapses}` reply.

Each example has its own `go.mod` with a local `replace` directive back to
this checkout, and is exercised by the repo-level `TestExamplesBuild` golden
build test to keep them from drifting away from the public API.

## OpenTelemetry / Tracing

Ouvrier emits one span per pipeline, pipe, session, LLM call, tool call,
schema validation, and subagent task through an OTel-compatible `Tracer`
hook:

```go
runner := ovr.NewRunner(ovr.WithTracer(myTracer))
```

`myTracer` implements `ovr.Tracer` (a `StartSpan(ctx, name, attrs)` factory
that returns a `ovr.Span` with `End`, `SetAttribute`, and `RecordError`). Use
`ovr.NopTracer()` to disable tracing or in tests. The harness pairs
`*_started` events with their `*_completed` / `*_failed` counterparts
internally, so callers see one span per logical operation.

## Development

Run the default checks:

```sh
go test ./...
go vet ./...
```

Run race tests when touching concurrency, state, events, tools, or subagents:

```sh
GOCACHE=/tmp/go-build-cache go test -race ./...
```

Keep public behavior covered close to the owning package. Use small vertical
slices that compile and can be demonstrated.

## Security Model

Ouvrier assumes LLM output is untrusted:

- Tool calls pass through `ToolExecutor`.
- Privileged work passes through `PermissionPolicy`.
- Typed outputs pass through `ResultSchema`.
- Secrets are redacted before logs, events, state, and admin output.
- Side effects must be classified and authorized.
- Sandbox-backed capabilities must fail fast if requested guarantees cannot be
  enforced.

## Documentation

User-facing documentation:

- [Handbook](docs/handbook.md)
- [Public API Reference](docs/api.md)
- [Observability Guide](docs/observability.md)

## License

Ouvrier is licensed under the Apache License, Version 2.0. See `LICENSE`.

Created by Arnaud Guiovanna: https://aguiovanna.fr
