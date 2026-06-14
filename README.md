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

Ouvrier `main` includes the completed v0.1, v0.2, v0.3, and v0.4 milestone
backlog plus the v0.5 Ouvrier Agent Cockpit. The latest tagged release is
`v0.5.4` — the Pi/Claude-Code-class agentic cockpit plus a new **Ouvrier IDE**
(`ouvrier ide`): a terminal IDE to review/edit/save/rebuild a worker with live
**gopls** diagnostics, hover, go-to-definition, completion, an Ouvrier API **snippet palette**, and a Catppuccin
Macchiato theme. The cockpit keeps its streaming TUI, approval gate (prod
double-confirm), and Codex subscription auth. The public Go module path is:

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

What ships in the current codebase:

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
- Native provider token streaming through `EventStream` and `Reply(SSE())`,
  human-in-the-loop suspend/resume approvals, scoped persistent memory, model
  fallback chains, provider budgets, and cost accounting from pricing tables.
- Stream DLQ routing, replay tooling, broker backpressure/ack policy controls,
  and queue push terminals for Kafka, Redis, NATS, SQS, and HTTP(S).
- Protected admin endpoints (`/admin/health`, `/admin/status`,
  `/admin/plans`, `/admin/capabilities`, `/admin/events`, `/admin/traces`,
  `/admin/traces/<id>`, `/admin/trigger`, `/admin/approvals`,
  `/admin/streams/replay`), a Prometheus `/metrics` endpoint, plus a dev-mode
  trace viewer at `/dev`.
- OpenTelemetry-compatible `Tracer` hook (and a built-in native OTLP/HTTP
  span exporter via `WithOTLPExporter`) for pipeline, pipe, session, LLM,
  tool, schema, and subagent spans.
- The `ouvrier` CLI: `version`, `new`, `show`, `status`, `logs`, `trace`,
  `add agent|trigger|tool|skill`, `dev`, `build` (static cross-compile), and
  `deploy <env>|ssh|docker`, plus `operate` for local agentic worker building
  and `ide` for the worker review/edit IDE (gopls diagnostics + Ouvrier snippets).

Operational notes: trace persistence uses the configured `StateStore` (SQLite
by default), cost accounting depends on configured pricing and provider usage
metadata, and DLQ replay drains the runtime-retained copy for the configured
stream plan rather than consuming arbitrary broker DLQ topics.

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

## Install

Download a prebuilt binary (Linux/macOS, amd64/arm64) with the install script;
it fetches the latest release, verifies the checksum, and installs `ouvrier`:

```sh
curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/ouvrier/main/install.sh | sh
```

Pin a version with `OUVRIER_VERSION=v0.5.4` and the target directory with
`OUVRIER_BIN_DIR=~/.local/bin`. Re-running the script updates an existing
install in place.

Or install with `go install` (Go 1.25+); it places `ouvrier` in
`$(go env GOPATH)/bin`:

```sh
go install github.com/ArnaudGuiovanna/ouvrier/cmd/ouvrier@latest
ouvrier version
```

Use `@v0.5.4` instead of `@latest` to pin a specific release.

To build from a checkout instead (for contributing or running the tests):

```sh
git clone https://github.com/ArnaudGuiovanna/ouvrier.git
cd ouvrier
go test ./...
go install ./cmd/ouvrier
ouvrier version
```

During local development, generated projects use a `replace` directive pointing
back to this checkout. They also include `ouvrier.worker.json`, a small
machine-readable worker manifest for editor/agent integrations. A prototype Pi
extension lives in `integrations/pi-ouvrier/`; it discovers these manifests,
streams `/admin/events`, and exposes an Ouvrier Inbox inside Pi.

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
  `/admin/*` routes), which drains and reprocesses the runtime-retained copy
  for the configured stream plan and returns the replayed count. Broker DLQ
  targets are still published over the real transport; replaying an arbitrary
  broker topic or stream requires a dedicated consumer/custom DLQ integration.
  Dead-letter targets are credential-stripped in events/logs.

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
OUVRIER_ADMIN_TOKEN=
OUVRIER_STATE_BACKEND=sqlite
OUVRIER_STATE_PATH=.ouvrier/state.db
```

Admin endpoints are mounted under `/admin/*` for HTTP, webhook, cron-only,
stream-only, and mixed runtimes. `/admin/plans` and `/admin/capabilities`
return machine-readable compiled worker capabilities, and `/admin/events`
streams redacted events as JSONL or SSE for live IDE integrations. Set
`OUVRIER_ADMIN_TOKEN` outside local
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
GET  /admin/plans          # compiled trigger/step/terminal capabilities
GET  /admin/capabilities   # alias shape for integrations
GET  /admin/events         # JSONL/SSE event stream (?format=sse, ?follow=false)
GET  /admin/traces?last=N
GET  /admin/traces/{exec-id}
POST /admin/trigger        # returns exec_id/trace_id/session_id when scheduled
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
ouvrier add trigger --trigger "cron @every 1h" [--model "provider/model"] [--goal TEXT]
ouvrier add tool --name LoadTicket [--describe TEXT] [--readonly|--side-effecting|--idempotent KEY]
ouvrier add skill --name ticket-triage [--description TEXT]
ouvrier show [--dir .] [--json]
ouvrier dev [--dir .] [--addr :8080]
ouvrier status [--url http://127.0.0.1:8080] [--token TOKEN]
ouvrier logs   [--url URL] [--token TOKEN] [--last N]
ouvrier trace  <exec-id> [--url URL] [--token TOKEN]
ouvrier build  [--static] [--target os/arch] [--output PATH] [--dir .]
ouvrier operate [--dir .] [--agent codex|manual]
ouvrier operate --print "create a worker that receives POST /tickets"
ouvrier operate --mode json --prompt "review this worker"
ouvrier operate --mode rpc
ouvrier operate create-worker --yes --name NAME --trigger "POST /path" --model provider/model [--dir .]
ouvrier operate patch --goal TEXT [--agent codex|manual] [--dir .]
ouvrier operate review-worker [--scope whole_worker] [--agent codex|manual] [--dir .]
ouvrier operate fix-worker [--session ID] [--agent codex|manual]
ouvrier operate audit [--session ID] [--dir .]
ouvrier operate build [--session ID] [--target os/arch] [--allow-failed]
ouvrier operate transfer --env ENV [--session ID] [--target os/arch] [--allow-failed]
ouvrier deploy ENV [--env-file FILE] [--identity FILE] [--target os/arch] [--keep 5] [--yes]
ouvrier deploy ssh --host HOST [--dir .] (same flags; bypasses the registry)
ouvrier deploy docker [--dir .] [--image IMAGE] [--tag TAG] [--push]
ouvrier fleet ls
ouvrier fleet rm <name> [--host HOST]
ouvrier console [--addr 127.0.0.1:7333] [--fleet PATH] [--token TOKEN] [--no-open]
```

`ouvrier new` opens the Bubble Tea v2 project wizard. The wizard and
`ouvrier new --yes` support HTTP trigger strings such as `"POST /tickets"`.

`ouvrier operate` is the prompt-first local agent cockpit for manufacturing
workers. Run it from a worker or from a parent factory directory, then type the
worker you want: the cockpit can infer a plan, scaffold a normal Go worker,
load Ouvrier API context, ask the configured Codex/manual driver to patch code,
review findings, run audit gates, build the binary, and transfer through the
existing deploy engine. The same runtime powers the Bubble Tea UI, one-shot
`--print` and `--mode json`, and JSONL `--mode rpc`.

Codex auth follows the Pi-style ownership boundary: `/login codex` probes or
delegates to the local Codex CLI flow, while Ouvrier stores only profile
metadata, never Codex subscription tokens. Sessions and artifacts live under
`.ouvrier/operate/sessions/<id>/` (`transcript.jsonl`, `events.jsonl`,
`tool-calls.jsonl`, `auth_profile.json`, `goal.md`, `patch.json`,
`diff.patch`, `review.json`, `audit.json`, `build.json`, and
`transfer.json`) so a developer-operator can audit every step. The web console
remains the remote observation and approval surface for workers and fleets;
`operate` is the local construction harness.

The introspection commands (`show`, `status`, `logs`, `trace`) read from the
project filesystem (`pip.yaml`) or talk to a running worker through
`/admin/*` using `OUVRIER_ADMIN_TOKEN` for bearer auth. `ouvrier build`
compiles the worker; `--static` implies `CGO_ENABLED=0` with
`-ldflags="-s -w"` and supports `--target os/arch` for cross-compilation
(`modernc.org/sqlite` is pure Go, so static cross-builds work without a C
toolchain).

`ouvrier deploy <env>` (and its `deploy ssh --host` alias) is the agentless
SSH deploy: it cross-compiles a static binary, ships it into an immutable
`releases/<ts>-<sha>/` directory on hosts pinned in the committed
`ouvrier.known_hosts`, installs the env file and a hardened systemd unit,
atomically swaps the `current` symlink, health-gates the loopback admin
endpoint, and rolls back to the previous release on failure. Environments
(hosts, port, path, service, identity) come from the committed pip.yaml
`deploy:` block; see the handbook for the sudoers snippet and a CI example.
`ouvrier deploy docker` renders and builds a distroless container image that
includes `skills/` when present. `ouvrier fleet` lists or prunes the recorded deployments
inventory at `~/.config/ouvrier/deployments.json` (override with
`OUVRIER_FLEET_PATH` or `OUVRIER_CONFIG_DIR`); it is a secret-free cache for
tooling — live `/admin/health` is truth.

`ouvrier console` starts a loopback-only web console (default
`127.0.0.1:7333`, override with `--addr` / `OUVRIER_CONSOLE_ADDR`) that layers
over the exact same SSH tunnels and `/admin/*` APIs as the headless commands:
fleet overview with live tunnel state (`fleet ls` + tunnel status), a worker
detail view (status/plans/traces, an SSE event tail, approvals, manual
trigger — proxied to `/admin/*`), a fan-out overview (`status --all`), an event
fan-in (`logs`), and a streamed deploy (`deploy <env>`). Every console
capability has a headless CLI equivalent, enforced by a parity test. Security:
the bind is refused on a non-loopback address unless `OUVRIER_CONSOLE_INSECURE=1`;
a random 256-bit per-session token is printed in the URL fragment, held only in
browser memory, sent as `Authorization: Bearer`, and constant-time compared;
there are zero cookies, a Host-header allowlist and Origin rejection
(DNS-rebinding defense), `Cache-Control: no-store` on the API, and a strict CSP
plus `X-Frame-Options: DENY` on the SPA. The worker admin token is fetched into
memory by the tunnel manager and injected server-side — it never reaches the
browser. The SPA is vendored Preact + HTM ESM served from `//go:embed`: `go
build` alone produces the whole console, with no npm/bundler in the repo or CI.
`--fleet` overrides the inventory path and `--no-open` suppresses the browser
auto-open.

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
