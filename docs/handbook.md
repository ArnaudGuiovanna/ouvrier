# Ouvrier Handbook

<!-- This single-file handbook intentionally exceeds the usual preferred doc
length because Ouvrier usage is documented end to end in one user-facing guide. -->

This handbook explains how to build, run, observe, and deploy Ouvrier workers.

Ouvrier is a Go framework for small agentic middleware services. You declare
where work comes from, what an agent should do, which tools it may call, and
where the outcome goes. Ouvrier turns that declaration into a service with typed
results, governed tools, durable state, traces, admin endpoints, and deployment
commands.

## Version And Requirements

- Ouvrier version: current `main` includes v0.1 and v0.2 milestone work; the
  latest tagged release remains `v0.1.0` until v0.2 is tagged.
- Go version: Go 1.25 or newer.
- Public module path: `github.com/ArnaudGuiovanna/ouvrier`.
- Runtime package name: `ovr`.
- CLI binary: `ouvrier`.

Install the CLI from a checkout:

```sh
git clone git@github.com:ArnaudGuiovanna/ouvrier.git
cd ouvrier
go test ./...
go install ./cmd/ouvrier
ouvrier version
```

Generated projects import Ouvrier from `github.com/ArnaudGuiovanna/ouvrier`.
When the generator can see a local checkout, it adds a `replace` directive so
the generated worker builds against that checkout during development.

## Mental Model

Every worker is:

```txt
From -> Pipe -> Pipe -> Reply / Push / Sink
```

- `From` receives work from HTTP, cron, webhooks, or streams.
- `Pipe` runs one LLM-backed agent step with a goal, model, tools, skills, MCP,
  Bash, subagents, budgets, retries, and typed output.
- `Reply`, `Push`, and `Sink` decide what happens to the final result.

The public model is intentionally small: trigger, goal, tools, outcome.
Internally, every `Pipe` runs through the harness: session, result schema, tool
executor, permission policy, sandbox, hooks, event stream, state store,
provider adapter, retries, budgets, and subagent task execution.

## Minimal Worker

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
	Priority string   `json:"priority"`
	Summary  string   `json:"summary"`
	Tags     []string `json:"tags"`
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

Run it:

```sh
export ANTHROPIC_API_KEY=...
export OUVRIER_ENV=dev
go run .
```

Call it:

```sh
curl -X POST http://localhost:8080/tickets/T-123 \
  -H 'Content-Type: application/json' \
  -d '{"message":"user cannot sign in"}'
```

With path parameters, the first pipe sees JSON like:

```json
{
  "path_params": {"id": "T-123"},
  "body": {"message": "user cannot sign in"}
}
```

Without path parameters, the first pipe receives the raw request body.

## Project Layout

A generated project looks like this:

```txt
my-worker/
  main.go
  go.mod
  go.sum
  pip.yaml
  ouvrier.worker.json
  .env.example
  .gitignore
  README.md
  skills/
  tools/
```

Use `.env.example` as the team-visible template. Keep `.env` local and out of
Git. `pip.yaml` supplies project metadata used by `ouvrier show`, `ouvrier
build`, and deployment commands. `ouvrier.worker.json` is a small
machine-readable manifest (`name`, `description`, `events`, `outcomes`,
`admin_url`) intended for editor and agent integrations. The Pi extension in
`integrations/pi-ouvrier/` uses it to discover workers, stream admin events, and
show asynchronous feedback in an Ouvrier Inbox.

## Creating Projects

Interactive scaffold:

```sh
ouvrier new
```

The TUI asks for a project name, trigger, model, and review confirmation, then
generates the project. The trigger step accepts HTTP, cron, webhook, and stream
forms.

Non-interactive scaffold:

```sh
ouvrier new \
  --yes \
  --name ticket-triage \
  --trigger "POST /tickets/{id}" \
  --model "anthropic/claude-sonnet-4-6" \
  --dir /tmp
```

The scaffold accepts every supported trigger category via `--trigger`:

- HTTP routes: `"POST /tickets"`, `"GET /health"` (terminates with `Reply(JSON)`).
- Cron: `"0 6 * * *"` or `"cron @every 1h"` (terminates with `Sink(Log())`).
- Webhook: `"webhook github"` (terminates with `Sink(Log())`).
- Stream: `"stream kafka://tickets"` (terminates with `Sink(Log())`).

## Triggers

### HTTP

```go
ovr.From("GET /health")
ovr.From("POST /tickets/{id}")
```

HTTP routes support `GET` and `POST`. Path parameters use Go `net/http`
patterns such as `{id}` and are exposed to the pipeline under `path_params`.
Request bodies are capped at 1 MiB.

### Cron

```go
ovr.From(ovr.Cron("0 6 * * *"))
```

Cron plans run on the schedule and usually terminate with `Push` or `Sink`.
Use `/admin/trigger` to fire a cron plan manually in development or operations.

### Webhook

```go
ovr.From(ovr.Webhook("github"),
	ovr.VerifySignature("GITHUB_WEBHOOK_SECRET", "X-Hub-Signature-256"),
	ovr.IdempotencyKey("X-GitHub-Delivery"),
)
```

Webhook routes are mounted at `POST /webhooks/<provider>`. The pipe input is:

```json
{"trigger":"webhook","provider":"github","body":{}}
```

`VerifySignature` checks HMAC-SHA256 signatures. Headers may be either raw hex
or prefixed with `sha256=`. The comparison is constant-time (`hmac.Equal`).

The signature is computed over the raw request body only, matching the common
provider convention (GitHub, generic HMAC). It proves authenticity but not
freshness: a captured, validly-signed request can be replayed. Pair
`VerifySignature` with `IdempotencyKey` on a stable per-delivery header (e.g.
`X-GitHub-Delivery`) so a replayed delivery is rejected as a duplicate. Do not
rely on signature verification alone for replay protection.

### Stream

```go
ovr.From(ovr.Stream("nats://127.0.0.1:4222/tickets"))
ovr.From(ovr.Stream("redis://127.0.0.1:6379/tickets"))
ovr.From(ovr.Stream("kafka://localhost:9092/tickets"))
```

Streams run through the same harness as HTTP and webhook triggers. Message IDs
are reserved in the state store for idempotency when available. Failed stream
deliveries emit a dead-letter event in the event stream so operators can route
or replay them intentionally. Broker DLQ targets are published over the real
transport; admin replay drains the runtime-retained DLQ copy for the configured
stream plan, not an arbitrary broker topic or stream.

### Trigger Options

```go
ovr.From("POST /tickets",
	ovr.WorkerPool(20),
	ovr.IdempotencyKey("Idempotency-Key"),
	ovr.VerifySignature("WEBHOOK_SECRET", "X-Signature"),
)
```

`WorkerPool` returns `429 worker_pool_full` when the route is saturated.
`IdempotencyKey` requires a state store and returns `202 duplicate_idempotency_key`
for repeated deliveries.

## Pipes

A pipe must have a goal and an explicit model:

```go
ovr.Pipe("Summarize the request.",
	ovr.Model("openai/gpt-5.1"),
	ovr.Timeout("30s"),
	ovr.MaxTokens(20000),
	ovr.MaxCostUSD(2.00),
	ovr.Retry(2, ovr.ExponentialBackoff()),
	ovr.Output[Summary](),
)
```

Supported model prefixes are `anthropic/`, `openai/`, `ollama/`, `mistral/`,
`gemini/`, and `vllm/`. Provider credentials are loaded from environment
variables:

```txt
ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL
OPENAI_API_KEY, OPENAI_BASE_URL
MISTRAL_API_KEY, MISTRAL_BASE_URL
GEMINI_API_KEY, GEMINI_BASE_URL
OLLAMA_BASE_URL
VLLM_API_KEY, VLLM_BASE_URL
```

`NoCache()` disables provider prompt-cache hints for one pipe.
`SequentialTools()` forces tool calls from a provider turn to run one at a time.

## Typed Output

`Output[T]()` generates a strict JSON Schema from `T`, injects the schema into
the harness prompt, validates the final result, records violations, and can run
bounded repair attempts when enabled on the runner. Fenced `json` blocks and
short prose around a JSON object or array are stripped before validation; the
typed schema still decides whether the result is accepted.

```go
type Summary struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

runner := ovr.NewRunner(ovr.WithSchemaRepairAttempts(1))
err := runner.Run(":8080",
	ovr.From("POST /summaries"),
	ovr.Pipe("Summarize the payload.",
		ovr.Model("anthropic/claude-sonnet-4-6"),
		ovr.Output[Summary](),
	),
	ovr.Reply(ovr.JSON[Summary]()),
)
```

If the last pipe omits `Output[T]()` but the terminal is `Reply(JSON[T]())`,
Ouvrier applies the reply schema to the last pipe. If both are present, their
schemas must match.

## Go Tools

Tools are ordinary Go functions:

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
	ovr.ToolTimeout("10s"),
)
```

Tool rules:

- First parameter must be `context.Context`.
- Return either `error` or `(value, error)`.
- Arguments and results should be JSON-serializable.
- Struct fields should have `json` tags.
- Arguments are validated before the function runs.
- Panics are converted into structured tool errors.

Classify every tool:

```go
ovr.ReadOnly()
ovr.Idempotent("ticket.id")
ovr.SideEffecting("email")
ovr.RequiresApproval()
```

Read-only and idempotent tools can be retried safely. Side-effecting tools
require permission. Tools without an explicit classification are treated as
side-effecting and non-idempotent.

## Permissions

The default policy allows read-only tools, idempotent tools with a key,
declared subagents, and redacted log sinks. It denies non-idempotent side
effects and targeted output actions. Tools marked `RequiresApproval()` are
blocked without an approval flow; with a configured approval store/runtime, the
tool call suspends and can resume after `POST /admin/approvals/<id>` approves
it.

Allow a side-effecting Go tool by label:

```go
runner := ovr.NewRunner(
	ovr.WithPermissionPolicy(ovr.AllowSideEffects("email")),
)
```

Allow targeted output side effects:

```go
runner := ovr.NewRunner(
	ovr.WithPermissionPolicy(
		ovr.AllowSideEffectTargets("webhook", "https://example.com/result"),
	),
)
```

Common target-scoped labels are `webhook` for webhook push, `queue` for queue
push, `file` for file sinks, `mcp:<server-name>` for MCP calls, and `process`
or `filesystem` for Bash.

For custom governance, implement `ovr.PermissionPolicy`.

## Skills

Attach reusable Markdown instructions:

```go
ovr.Pipe("Triage the ticket.",
	ovr.Model("anthropic/claude-sonnet-4-6"),
	ovr.Skill("ticket-triage"),
)
```

Skill files live at `skills/<name>/SKILL.md` and must contain `name` and
`description` frontmatter:

```markdown
---
name: ticket-triage
description: Triage support tickets by urgency and product area.
---

# Instructions

Classify urgency, summarize the issue, and return tags.
```

The Markdown body is injected into the system prompt in declaration order.
Support files are accessed only through governed filesystem capabilities.

## MCP

Connect an MCP server as a governed tool set:

```go
ovr.Pipe("Inspect Moodle activity.",
	ovr.Model("anthropic/claude-sonnet-4-6"),
	ovr.MCP("moodle-mcp"),
)
```

Ouvrier reads MCP server configuration from environment variables derived from
the name, for example `MOODLE_MCP_URL` and `MOODLE_MCP_TOKEN`. MCP calls pass
through the tool executor, permission policy, event stream, and state store.

## Bash

Bash is available as a sandboxed tool named `bash`:

```go
ovr.Pipe("Inspect the workspace.",
	ovr.Model("anthropic/claude-sonnet-4-6"),
	ovr.Bash(
		ovr.Sandbox("./workspace", ovr.AllowEnv("PATH")),
		ovr.BashTimeout("5s"),
		ovr.BashMaxOutputBytes(32768),
	),
)
```

By default, Ouvrier requires platform isolation for workspace, environment,
process, and network boundaries. Startup fails if those guarantees cannot be
enforced. `UnsafeBashHostExecution()` is available for trusted local workloads
and still keeps permission checks, environment allowlisting, timeouts, working
directory resolution, and output bounds.

## Composition

Sequential pipes pass output to the next pipe:

```go
ovr.Pipe("Extract facts.", ovr.Model("anthropic/claude-haiku-4-5")),
ovr.Pipe("Write the final answer.", ovr.Model("anthropic/claude-sonnet-4-6")),
```

`Parallel` fans out the same input and returns ordered outcomes:

```go
ovr.Parallel(
	ovr.Pipe("Check quality.", ovr.Model("anthropic/claude-haiku-4-5")),
	ovr.Pipe("Check policy.", ovr.Model("anthropic/claude-haiku-4-5")),
)
```

`Map` expects the previous output to be a JSON array and processes items with a
bounded concurrency:

```go
ovr.Map(
	ovr.Concurrency(10),
	ovr.Pipe("Score one item.", ovr.Model("anthropic/claude-haiku-4-5")),
)
```

`PartialOK()` changes `Parallel`, `Map`, and `SubAgent` failure behavior to
return ordered partial outcomes instead of failing the parent immediately.

## Subagents

A subagent exposes a child pipeline as a governed tool:

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

Subagents run through `ToolExecutor`, inherit budgets and policy, create child
sessions, propagate cancellation, enforce a maximum depth, and attach child
events to the parent trace. `MaxParallel` cannot exceed the runtime safety cap.

## Terminals

### Reply

```go
ovr.Reply(ovr.JSON[T]())
ovr.Reply(ovr.SSE())
ovr.Reply(ovr.Accepted())
```

`JSON[T]` validates and returns typed JSON. HTTP responses wrap the raw output
as `{"status":"ok","output":"..."}`. `SSE` streams Ouvrier events and output as
Server-Sent Events. `Accepted` returns `202 accepted` and runs the pipeline in
the background.

### Push

```go
ovr.Push(ovr.Webhook("https://example.com/result"))
ovr.Push(ovr.Queue("https://queue.example.com/jobs"))
ovr.Push(ovr.Queue("nats://127.0.0.1:4222/results"))
```

Push terminals are output tools and require permission for their target.

### Sink

```go
ovr.Sink(ovr.Log())
ovr.Sink(ovr.File("./out/result.json"))
```

Log sinks are redacted and allowed by default. File sinks require a runner
sandbox and a target-scoped file permission.

## Runner Configuration

`ovr.Run` uses secure defaults. Use `NewRunner` when you need custom state,
policy, hooks, sandbox, schema repair, or tracing:

```go
hooks := ovr.NewHooks()
_ = hooks.Register(ovr.EventToolCallFailed, func(ctx context.Context, event ovr.Event) (ovr.Event, error) {
	return event, nil
})

runner := ovr.NewRunner(
	ovr.WithHooks(hooks),
	ovr.WithTracer(ovr.NopTracer()),
	ovr.WithSandbox(ovr.Sandbox("./data", ovr.AllowEnv("PATH"))),
	ovr.WithSchemaRepairAttempts(1),
)
```

The public `StateStore` interface supports executions, sessions, idempotency,
events, and schema violations. By default, `Run` uses SQLite at
`.ouvrier/state.db`.

Environment:

```txt
OUVRIER_STATE_BACKEND=sqlite  # sqlite, postgres, or memory
OUVRIER_STATE_PATH=.ouvrier/state.db
OUVRIER_STATE_DSN=...         # postgres connection string; secret, never logged
OUVRIER_STATE_MIGRATE=auto    # auto (default) applies schema migrations at startup; off only verifies
OUVRIER_DURABLE_RUNS=1        # opt-in step-checkpoint run journal (default off); refuses the memory backend
OUVRIER_DURABLE_RETENTION=72h # how long failed/suspended run journals are kept (default 72h)
OUVRIER_ADMIN_TOKEN=...
OUVRIER_ENV=dev               # enables unauthenticated admin only when no token is set
OUVRIER_ADMIN_ADDR=127.0.0.1:9090 # optional: move /admin/*, /metrics, /dev to a dedicated loopback listener
OUVRIER_METRICS_PUBLIC=1      # optional: when split, keep /metrics also on the public port
```

### Schema Migrations And DML-Only Roles

With the default `OUVRIER_STATE_MIGRATE=auto`, the worker applies pending
schema migrations at startup. Postgres migrations run inside one transaction
serialized by an advisory lock, so concurrent replicas starting against the
same database cannot race the DDL.

Hardened production deployments often give the worker a DML-only database
role. Set `OUVRIER_STATE_MIGRATE=off` for those workers: startup then only
verifies the schema version and refuses to start while migrations are
pending — it never runs DDL. Apply migrations explicitly with a DDL-capable
role instead:

```sh
OUVRIER_STATE_BACKEND=postgres OUVRIER_STATE_DSN=... ouvrier state migrate
```

`ouvrier state migrate` prints each schema version it applies, is a no-op
when the schema is already current, and is safe to run concurrently thanks to
the same advisory lock. It works for SQLite too (`OUVRIER_STATE_PATH`), where
it stamps `PRAGMA user_version`. Ouvrier v0.3.x migrations are additive-only
— new tables, columns, and indexes, never destructive changes — so a freshly
migrated schema keeps serving the previous binary during a rolling deploy.

## Testing Workers

Test a worker end to end in a Go test without a real model or a network
listener. `ovr.WithProvider` injects an LLM provider, `Runner.Handler` (or the
package-level `ovr.Handler`) compiles your nodes into an `http.Handler` for
`httptest`, and the `ovrtest` package supplies a scripted provider that returns
canned turns. The handler defaults to an in-memory state store, so tests touch
neither disk nor environment.

```go
func TestTriage(t *testing.T) {
	provider := ovrtest.NewProvider(
		ovrtest.Text(`{"priority":"high","summary":"cannot log in"}`),
	)
	handler, err := provider.Handler(
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Output[Triage](),
		),
		ovr.Reply(ovr.JSON[Triage]()),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/tickets/42", "application/json", strings.NewReader(`{}`))
	// resp body is {"status":"ok","output":"<your JSON[T] as a string>"}
}
```

Script a tool-using pipe turn by turn: a turn carrying a tool call makes the
harness run that tool and ask the provider again.

```go
provider := ovrtest.NewProvider(
	ovrtest.Tool("load_ticket", `{"id":"42"}`),                 // turn 1: call the tool
	ovrtest.Text(`{"priority":"low","summary":"resolved"}`),    // turn 2: final answer
)
```

`provider.Requests()` and `provider.CallCount()` expose what the worker sent the
model, so you can assert on the prompt and the number of turns.

## Evals And Regression Gates

The `ovreval` package runs a golden dataset of cases through a worker — the same
compiled pipeline `Run` serves — and checks the typed output against assertions,
producing a pass rate you can gate CI on. It builds on `Handler`, so an eval
suite needs no listener; pair it with a scripted provider for deterministic
runs, or a live provider to evaluate against a real model.

```go
func TestTriageEval(t *testing.T) {
	suite := ovreval.New(
		ovr.NewRunner(ovr.WithProvider(provider)),
		ovr.From("POST /tickets/{id}"),
		ovr.Pipe("Triage the support ticket.",
			ovr.Model("anthropic/claude-sonnet-4-6"),
			ovr.Output[Triage](),
		),
		ovr.Reply(ovr.JSON[Triage]()),
	)

	report := suite.RunT(t,
		ovreval.Case{
			Name: "outage is high priority",
			Path: "/tickets/1",
			Body: `{"subject":"site is down"}`,
			Assert: []ovreval.Assertion{
				ovreval.WantStatus(200),
				ovreval.OutputField("priority", "high"),
				ovreval.OutputContains("down"),
			},
		},
	)
	if report.PassRate() < 1.0 {
		t.Fatalf("eval pass rate %.0f%% below gate", report.PassRate()*100)
	}
}
```

Assertions cover the HTTP status (`WantStatus`), JSON output fields
(`OutputField`, compared JSON-normalized), substrings (`OutputContains`), and
anything else via `Custom`. `Run` returns a `Report` (`Passed`, `Failed`,
`PassRate`) for non-test callers; `RunT` reports each failing case through
`*testing.T` so a dataset reads like a table test.

## Observability And Admin

Admin endpoints:

```txt
GET  /admin/health
GET  /admin/status
GET  /admin/plans
GET  /admin/capabilities
GET  /admin/events              # JSONL by default, SSE with ?format=sse
GET  /admin/traces?last=N
GET  /admin/traces/<exec-id>
POST /admin/trigger             # returns exec_id/trace_id/session_id when scheduled
GET  /dev
```

Outside `OUVRIER_ENV=dev`, set `OUVRIER_ADMIN_TOKEN` and send
`Authorization: Bearer <token>`. All admin output is redacted before it leaves
the process.

By default the admin surface shares the public port. Set `OUVRIER_ADMIN_ADDR`
(e.g. `127.0.0.1:9090`) and `Run` serves `/admin/*`, `/metrics`, and `/dev` on
a dedicated second listener instead; the public port answers 404 for them
while trigger routes are unaffected. The admin bind must be loopback — a
non-loopback `OUVRIER_ADMIN_ADDR` refuses startup unless
`OUVRIER_ADMIN_INSECURE=1` overrides it. Token enforcement on the dedicated
listener is identical. When split, `OUVRIER_METRICS_PUBLIC=1` keeps `/metrics`
also on the public port (same bearer auth) for Prometheus scrapers that cannot
reach the loopback admin listener. `ovr.Handler` ignores `OUVRIER_ADMIN_ADDR`
and always returns the combined handler, so tests keep driving trigger and
admin routes through one seam.

Trigger a route through admin:

```sh
curl -X POST http://localhost:8080/admin/trigger \
  -H "Authorization: Bearer $OUVRIER_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"trigger":"http","method":"POST","path":"/tickets/T-123","body":{"message":"login broken"}}'
```

Use the CLI:

```sh
ouvrier status --url http://localhost:8080
ouvrier logs --last 50
ouvrier trace <exec-id>
```

Install a tracer with `WithTracer`. Ouvrier emits spans for pipeline, pipe,
session, LLM call, tool call, schema validation, and subagent task lifecycles.

## CLI Reference

```txt
ouvrier version
ouvrier new
ouvrier new --yes --name NAME --trigger "POST /path" --model provider/model
ouvrier add agent --name NAME --model provider/model [--goal TEXT]
ouvrier add trigger --trigger "cron @every 1h" [--model provider/model] [--goal TEXT]
ouvrier add tool --name LoadTicket [--describe TEXT] [--readonly|--side-effecting|--idempotent KEY]
ouvrier add skill --name ticket-triage [--description TEXT]
ouvrier show [--dir .] [--json]
ouvrier dev [--dir .] [--addr :8080]
ouvrier build [--dir .] [--output PATH] [--target linux/amd64] [--static]
ouvrier status [--url URL] [--token TOKEN]
ouvrier logs [--url URL] [--token TOKEN] [--last N]
ouvrier trace <exec-id> [--url URL] [--token TOKEN]
ouvrier server trust HOST [--fingerprint SHA256:...] [--rotate] [--port 22] [--dir .]
ouvrier deploy ssh --host HOST [--user USER] [--port 22] [--path PATH] [--service NAME] [--identity FILE]
ouvrier deploy docker [--image IMAGE] [--tag TAG] [--push] [--force]
ouvrier state migrate
```

`ouvrier dev` runs `go run .`, streams output, forwards shutdown signals, and
sets `OUVRIER_ADDR` when `--addr` is provided. Restart it after editing source
files.

## Build And Deploy

Build locally:

```sh
ouvrier build --static --target linux/amd64 --output ./bin/worker
```

Pin the server's SSH host keys before the first deploy. `ouvrier server
trust` runs `ssh-keyscan`, shows the SHA256 fingerprint (the ed25519 key when
the server offers one, otherwise the first scanned key), and writes every
scanned key line to a committed `ouvrier.known_hosts` at the project root —
host public keys are not secrets, so committing them shares the trust
decision with the team and CI:

```sh
ouvrier server trust app.example.com            # interactive confirm
ouvrier server trust app.example.com \
  --fingerprint SHA256:f/+IMT34E8qsxk2X...      # non-interactive (CI)
git add ouvrier.known_hosts && git commit -m "trust app.example.com"
```

Every deploy then runs ssh/scp with `-o UserKnownHostsFile=ouvrier.known_hosts
-o StrictHostKeyChecking=yes -o BatchMode=yes` and password authentication
disabled. Deploying to an unpinned host fails before any remote command; a
changed host key is a hard error — re-pin deliberately with
`ouvrier server trust --rotate HOST`.

Deploy over SSH:

```sh
ouvrier deploy ssh \
  --host app@example.com \
  --path /opt/ouvrier/ticket-triage \
  --service ouvrier-ticket-triage \
  --admin-token "$OUVRIER_ADMIN_TOKEN"
```

The SSH deploy builds a static Linux binary, uploads the binary, `.env`, and
`skills/` runtime assets when present, renders a systemd unit, restarts the
service, health checks `/admin/health`, and rolls back to the previous binary
if the health check fails.

Build a distroless container image:

```sh
ouvrier deploy docker --image registry.example.com/ticket-triage --tag 0.1.0 --push
```

## Security Checklist

- Set `OUVRIER_ADMIN_TOKEN` anywhere the worker is reachable by other machines.
- Use `VerifySignature` for public webhook routes.
- Use `IdempotencyKey` for retries from upstream systems.
- Mark tools `ReadOnly`, `Idempotent`, or `SideEffecting` deliberately.
- Keep `.env` out of Git and never embed secrets into binaries.
- Use `WithPermissionPolicy` before enabling side effects, push targets, file
  sinks, MCP, or Bash.
- Use `Sandbox` for file sinks and filesystem-bound capabilities.
- Treat LLM output as untrusted even when using typed schemas.

## Troubleshooting

| Symptom | Meaning | Fix |
| --- | --- | --- |
| `provider_not_configured` | The model prefix has no configured provider. | Set the matching provider env vars or change `Model`. |
| `pipeline_execution_failed` | The harness, tool, provider, terminal, or schema validation failed. | Check `ouvrier trace <exec-id>` and admin events. |
| `pipeline_execution_incomplete` | The provider did not return a usable final result. | Inspect provider/tool events and schema violations. |
| `worker_pool_full` | `WorkerPool` is saturated. | Raise the limit or retry later. |
| `signature_missing` or `signature_invalid` | HMAC verification failed. | Check the signing secret, header, and raw body. |
| `duplicate_idempotency_key` | The request was already reserved or completed. | Reuse the original execution result or send a new idempotency key. |
| `admin_token_required` | Admin token is missing outside dev mode. | Set `OUVRIER_ADMIN_TOKEN` and pass a bearer token. |

## Reference Examples

- `examples/ticket-triage`: support ticket triage with a Go tool, skill, and
  typed JSON reply.
- `examples/moodle-fsrs`: Moodle-style review scheduling shape with a typed
  tool result and JSON reply.

Both examples have local `replace` directives and are covered by repository
build tests.
