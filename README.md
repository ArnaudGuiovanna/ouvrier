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

	ovr "ouvrier"
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

Ouvrier v0.1 is under active implementation. The runtime and harness foundations
are in place, but the release is not complete yet. The public Go module path is:

```txt
module github.com/ArnaudGuiovanna/ouvrier
```

Generated projects import the framework under that path. During local
development before the repository is published, scaffolded projects add a
`replace github.com/ArnaudGuiovanna/ouvrier => <local-checkout>` directive so
they keep building against the working tree.

Current working foundations include:

- HTTP triggers, Cron triggers, and compiled runtime plans.
- Sequential `Pipe` execution through the harness path.
- Go tools through `ToolExecutor` and `PermissionPolicy`.
- Skill loading and deterministic prompt injection from `skills/<name>/SKILL.md`.
- Typed `Output[T]()` and `Reply(JSON[T]())` schema validation.
- Memory and SQLite state stores.
- Event stream, hooks, persisted trace events, and early HTTP admin endpoints.
- Provider adapters for the v0.1 model prefixes.
- `Parallel`, `Map`, bounded `Concurrency`, and `PartialOK` composition.
- Bash capability execution through `ToolExecutor` with a platform sandbox probe
  and explicit unsafe host fallback.
- HTTP-only CLI scaffold and a Bubble Tea TUI preview.
- Governed `SubAgent` foundations in active development.

The v0.1 backlog remains broader: webhook/stream runtime hardening, mixed
runtime hardening, broader queue/backpressure semantics, full CLI workflows,
deployment, production admin/trace viewer hardening, docs, examples, remaining
Bash hardening, DLQ behavior, and release gates.

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
Today it generates HTTP-triggered workers only:

```sh
ouvrier new \
  --yes \
  --name ticket-triage \
  --trigger "POST /tickets/{id}" \
  --model "anthropic/claude-sonnet-4-6" \
  --dir /tmp
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
Mixed HTTP, Webhook, Cron, and Stream runtime plumbing exists, but stream
transports are still experimental: stream URIs validate and receiver boundaries
exist, with minimal NATS, Redis Streams, and Kafka receiver code. Production
ack/retry, backpressure, and durable DLQ behavior are not ready yet. Redis
Streams with `WorkerPool > 1` can replay already-acked messages after an
earlier concurrent nack; keep Redis stream workers single-concurrency unless a
state store idempotency key fully protects side effects.

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
validates final JSON strictly. Violations are recorded in `EventStream` and
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
ovr.Sink(ovr.Log())
ovr.Sink(ovr.File("./out/result.json"))
```

`Reply(JSON[T]())`, `Reply(SSE())`, `Reply(Accepted())`, webhook push, NATS/HTTP
queue push, log sink, and file sink have current runtime coverage. Broader queue
protocols and production delivery semantics, including backpressure, retry, and
DLQ behavior, are tracked for v0.1.

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
```

Runtime/admin configuration:

```sh
PIP_ADMIN_TOKEN=
OUVRIER_STATE_BACKEND=sqlite
OUVRIER_STATE_PATH=.ouvrier/state.db
```

Early admin endpoints are mounted under `/admin/*` for HTTP, webhook, cron-only,
stream-only, and mixed runtimes. Set `PIP_ADMIN_TOKEN` outside local
development when exposing a worker; the full v0.1 admin/status/trace viewer
surface is still in progress.

## Observability

Ouvrier records structured events for pipeline, pipe, session, LLM, tool,
permission, schema, budget, sink, and subagent task activity.

Useful early endpoints in the current runtime:

```txt
GET  /admin/health
GET  /admin/status
GET  /admin/traces?last=N
GET  /admin/traces/{exec-id}
POST /admin/trigger
```

Events are redacted before persistence or admin output. The current endpoints
are diagnostics, not the complete v0.1 admin experience.

## CLI

Current commands:

```sh
ouvrier version
ouvrier new --yes --name NAME --trigger "POST /path" --model "provider/model"
ouvrier new
```

`ouvrier new --yes` currently supports HTTP trigger strings only, such as
`"POST /tickets"`. The no-flag `ouvrier new` command opens a Bubble Tea v2
preview, but it does not generate a project yet. The full v0.1 CLI backlog
includes interactive `new`, `add agent`, `add tool`, `add skill`, `show`, `dev`,
`build`, `deploy`, `status`, `logs`, and `trace`.

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

Primary implementation sources:

- `specs.md`
- `ouvrier-doc-v0.1.pdf`
- GitHub milestone `v0.1`

If the spec and documentation disagree, treat that as a product/spec bug to
resolve explicitly. Do not silently shrink v0.1 scope.

## License

Ouvrier is licensed under the Apache License, Version 2.0. See `LICENSE`.

Created by Arnaud Guiovanna: https://aguiovanna.fr
