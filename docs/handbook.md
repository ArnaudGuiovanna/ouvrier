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

- Ouvrier version: current `main` includes the v0.1-v0.5 shipped work; the
  latest tagged release is `v0.5.5`.
- Release status: post-`v0.5.5` changes on `main` are an active stabilization
  line. They are not a new stable release until the complete repository gates
  have passed.
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
`admin_url`) intended for editor and agent integrations.

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

Durable mode fingerprints the complete compiled replay contract (trigger,
steps, tools, budgets, schemas, retries, and terminal destination) together
with the SHA-256 identity of the exact worker executable. A journal created by
a different contract or binary is abandoned with an observable plan-hash
mismatch instead of being replayed under new Go handler/tool code. This is an
intentional fail-closed migration rule: deploying a rebuilt binary abandons
older journals even if its plan looks identical, while byte-identical replicas
can recover each other's work. No database migration is required. Startup also
refuses durable mode if it cannot read and fingerprint its executable. HTTP and
Webhook plans must contain at least one `Pipe` while durable mode is enabled;
startup rejects a direct zero-step terminal because that path cannot yet
guarantee crash-safe terminal intent. Zero-step Cron and Stream plans do use
the durable runner and remain supported.

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
listener. `ovr.Provider` is the injectable LLM boundary, `ovr.WithProvider`
installs one implementation. `Runner.Handler` (or the
package-level `ovr.Handler`) compiles your nodes into an `http.Handler` for
`httptest`, while the `ovrtest` package supplies a scripted provider that returns
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

### Fleet CLI

`ouvrier status`, `ouvrier logs`, and `ouvrier trace` target a single local
worker via `--url`/`--token` by default. To reach deployed workers instead,
add `--worker NAME` (one worker) or `--all` (every worker in the deployments
inventory). Fleet mode resolves targets from
`~/.config/ouvrier/deployments.json`, opens a one-shot SSH tunnel per worker
(host keys pinned against the project's `ouvrier.known_hosts`, admin token
fetched over SSH and held in memory only), runs the same admin call, and
prefixes every output line with the worker name. `--all` fans out
concurrently with a per-worker timeout: each worker's result is printed under
a `=== <name> [<tunnel-state>] ===` header, and the command exits nonzero if
any worker failed while still printing the successes. `status` fleet mode also
shows each worker's tunnel state and `cron_leases`. `--url` cannot be combined
with `--worker`/`--all`, and `--worker` and `--all` are mutually exclusive.
SSH remains the only operator credential — admin ports never leave the host's
loopback, and no token is ever printed.

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
ouvrier operate [--dir .] [--agent codex|manual] [--codex-mode auto|exec|app-server] [--auto-safe]
ouvrier operate --print "create a worker that receives POST /tickets"
ouvrier operate --mode json --prompt "review this worker"
ouvrier operate --mode rpc
ouvrier operate create-worker --yes --name NAME --trigger "POST /path" --model provider/model [--dir .]
ouvrier operate patch --goal TEXT [--dir .] [--agent codex|manual]
ouvrier operate review-worker [--scope whole_worker] [--dir .] [--agent codex|manual]
ouvrier operate fix-worker [--session ID] [--agent codex|manual]
ouvrier operate audit [--session ID] [--dir .]
ouvrier operate build [--session ID] [--target linux/amd64] [--allow-failed]
ouvrier operate transfer --env ENV [--session ID] [--target linux/amd64] [--allow-failed]
ouvrier status [--url URL] [--token TOKEN] [--worker NAME | --all]
ouvrier logs [--url URL] [--token TOKEN] [--last N] [--worker NAME | --all]
ouvrier trace <exec-id> [--url URL] [--token TOKEN] [--worker NAME | --all]
ouvrier server trust HOST [--fingerprint SHA256:...] [--rotate] [--port 22] [--dir .]
ouvrier deploy ENV [--env-file FILE] [--identity FILE] [--target GOOS/GOARCH] [--keep 5] [--yes] [--allow-shared-admin] [--unit-sandbox on|off]
ouvrier deploy ssh --host HOST [--user USER] [--port 22] [--path PATH] [--service NAME] [...same flags]
ouvrier deploy ssh --print-sudoers [--user USER] [--service NAME] [--path PATH]
ouvrier deploy rollback ENV [--env-file FILE] [--identity FILE] [--yes] [--allow-shared-admin]
ouvrier deploy rollback --host HOST [--user USER] [--port 22] [--path PATH] [--service NAME] [...same flags]
ouvrier deploy docker [--image IMAGE] [--tag TAG] [--push] [--force]
ouvrier state migrate
```

`ouvrier dev` runs `go run .`, streams output, forwards shutdown signals, and
sets `OUVRIER_ADDR` when `--addr` is provided. Restart it after editing source
files.

### Operate Harness

`ouvrier operate` is the prompt-first local construction cockpit for workers.
It is a Bubble Tea interface, visually aligned with the terminal-first Pi
workflow, but the workflow owner is Ouvrier: type a goal, let the cockpit
scaffold or select a worker, load Ouvrier API context, prompt Codex/manual
driver turns, review code, repair findings, run audit gates, and produce a
verified local artifact. Transfer remains as a compatibility command, but is
outside the active cockpit acceptance path while deployment work is paused.

The main path is conversational:

```sh
ouvrier operate
# then type:
# create a worker that receives POST /tickets, triages tickets, review it, and build it
```

The same runtime also supports one-shot and machine-readable modes:

```sh
ouvrier operate --print "create a worker that receives POST /tickets"
ouvrier operate --mode json --prompt "review this worker"
ouvrier operate --mode rpc
```

The non-interactive subcommands expose the same steps:

```sh
ouvrier operate patch --goal "add a read-only load_ticket tool"
ouvrier operate create-worker --yes --name inbox --trigger "POST /inbox" --model anthropic/claude-sonnet-4-6
ouvrier operate review-worker --scope governance_security
ouvrier operate fix-worker --session <id>
ouvrier operate audit --session <id>
ouvrier operate build --session <id>
ouvrier operate transfer --session <id> --env staging
```

Codex is accessed through the local Codex CLI driver (`--agent codex`), so
authentication stays owned by Codex (`/login codex` in the cockpit, or
`codex login` directly). Ouvrier stores only auth profile metadata, never Codex
subscription tokens. `--codex-mode auto` (the default) and
`--codex-mode exec` select the legacy Codex CLI exec driver behind Ouvrier's
deterministic governed planner. Because that transport is text-only, Ouvrier
never installs it as a structured model/tool loop. The structured
`--codex-mode app-server` transport is experimental and must be selected
explicitly; it is not a production default while its confinement and event
parity are still being proven. `--agent manual` keeps the same sessions and
gates without asking an agent to edit files.

Headless prompt, print, JSON, and RPC turns use the `manual` posture by default.
Read-only and idempotent tools may run, but a side-effecting or
`requires_approval` call is recorded as denied when no operator is attached.
Pass `--auto-safe` only when the caller intentionally authorizes
side-effecting cockpit tools. It does not weaken `requires_approval`, so a
headless transfer still fails closed. Interactive posture changes are explicit
operator actions and apply only to that UI session.

The model-visible file surface is deliberately narrow:

- `read_worker_file` returns at most 64 KiB and `write_worker_file` accepts at
  most 1 MiB of valid UTF-8;
- `list_worker_files` and case-sensitive literal `search_worker_files` are
  paginated, and cap traversal, metadata, per-file bytes, total bytes, queries,
  and result counts;
- `remove_worker_file` removes one regular file or one worker-internal symlink,
  never a directory or an external target;
- all four operations refuse `.git`, `.ouvrier`, `.env*` other than
  `.env.example`, PEM/key files, credential stores, external symlinks, and
  symlinks whose resolved target is sensitive.

#### Context, transport, and journal bounds

`/compact` persists a deterministic context checkpoint; it does not delete or
rewrite earlier transcript entries. The checkpoint summary is at most 64 KiB,
uses at most the latest 64 entries since the prior checkpoint, and becomes the
boundary for later model requests. A request that would include more than
2,048 post-checkpoint entries or 8 MiB fails with
`operate: model context requires compaction` and asks the operator to run
`/compact`. Compaction itself fails while a durable tool call lacks its matching
result, so it cannot erase an unresolved capability decision.

| Surface | Enforced bound |
| --- | --- |
| Ouvrier model loop | 16 model steps per operator turn; 16 tool calls per step and 64 total; 1 MiB assistant text; 1 MiB arguments per call; 256-byte call IDs/names; 8 KiB result returned to the model. |
| Transcript | 8 MiB per JSONL record and 64 MiB per file; readers/export refuse more than 100,000 entries; atomic Markdown export is capped at 128 MiB. |
| Cockpit event journal | 4 MiB per JSONL event; resume/replay accepts at most 64 MiB and 100,000 events. |
| HTTP providers | 8 MiB non-streaming JSON response; SSE is capped at 64 MiB total and 1 MiB per frame, with at most 8 MiB assembled text. Tool-capable SSE parsers cap arguments at 1 MiB, identities at 256 bytes, and a response at 128 tool calls. |
| Codex exec | 8 MiB / 100,000-line stdout, 1 MiB per line, 1 MiB accumulated assistant text, and 64 KiB stderr. |
| Codex app-server | 8 MiB per protocol message, 1 MiB / 4,096 items of accumulated response text, and 64 KiB stderr. |

An over-limit model stream is cancelled and the turn fails. An exhausted
16-step loop returns an explicit error rather than accepting the last partial
text. Resume may finish a valid final JSONL record that only lacks its newline,
or discard an invalid unterminated final fragment and record that recovery in
the durable transcript. It never repairs a corrupt middle record. Event
subscription is read-only and returns the journal error without a partial
replay. The automatic Codex signed-in check used for model selection has a
two-second CLI deadline; other auth operations remain bounded by their caller
context. Status/device-auth output capture is limited to 64 KiB and inherited
output pipes have a 250 ms wait bound.

#### Operator shell and repository inspection

The TUI's `!command` and silent-display `!!command` accelerators both call the
same `run_shell` tool. It is hidden from the model and classified
`requires_approval`, so every invocation prompts even in `auto-safe` posture.
On Linux the approved command runs for at most two minutes inside Bubblewrap:
all namespaces are unshared, capabilities are dropped, the ambient environment
is cleared and replaced with a fixed non-secret Go/shell environment, the host
network is absent, system tool directories are read-only, the selected worker
is the only writable host path, and `/tmp` is a private writable tmpfs.
Combined stdout/stderr is retained up to 64 KiB. There is no non-Linux or
no-Bubblewrap fallback. `!!` suppresses ordinary UI cards only; approval,
transcript, result, and tool-call audit records remain durable.

Git reads allow only `branch`, `diff`, `ls-files`, `rev-parse`, and `status`.
Hooks, fsmonitor, global attributes, external diff, textconv, pagers, and
interactive diff filters are disabled. Before diff/status, Ouvrier inspects up
to 128 `.gitattributes` files (64 KiB each, 1 MiB aggregate), plus the effective
`.git/info/attributes`, and refuses every non-empty `filter=` attribute. This
deliberately includes Git LFS (`filter=lfs`): remove the content-filter
attribute and provide ordinary materialized source before cockpit diff, patch,
or audit. No repository filter is executed as a fallback.

The source/evidence checks are bounded and fail closed rather than silently
claiming complete coverage:

- a source fingerprint accepts at most 100,000 files and 2 GiB;
- the sanitized-source secret scan accepts at most 10,000 files, 4 MiB per
  file, and 64 MiB in aggregate, independently of Git tracked/untracked state;
- one driver review context accepts at most 32 safe regular files, 64 KiB per
  file, and 256 KiB total. Whole-worker, governance, and deploy-readiness
  reviews fail if the complete requested scope cannot fit or contains a source
  symlink.

Patch driver, diff, and evidence-persistence failures transition to
`patch_failed`; audit execution, evidence, and failed-gate outcomes transition
to `audit_failed`. The durable session also stores a redacted `last_error` for
execution/persistence errors and a redacted transition reason, so a failed
operation does not remain in a success-looking `patching` or `auditing` state.
If writing that failure state itself fails, the persistence error is joined to
the original error and returned rather than hidden.

The production audit path does not execute candidate Go code directly on the
host. On Linux it requires a working Bubblewrap namespace sandbox; another OS,
a missing `bwrap`, or an unenforceable namespace fails the audit instead of
falling back to host execution. Dependency preparation runs offline in a
disposable stage. Then `go test`, `go vet`, and the static Linux/amd64 audit
build see read-only staged source, vendored dependencies, private cache/temp
directories, no network, `GOPROXY=off`, `GOWORK=off`, and no inherited
credential environment. Read-only metadata gates such as `gofmt -l`, manifest
coherence, and diff/secret inspection do not execute the candidate.

Audit and structured review results are valid only for the exact source
snapshot observed before and after their gates. The source digest includes the
worker tree, build/test/embed inputs from local `go.mod` replacements, the Go
toolchain identity, and the enforced `GOWORK=off` setting. Build recomputes the
same snapshot, binds the SHA-256 of the exact passing `audit.json`, and records
the binary SHA-256. A source, local dependency, toolchain, `audit.json`, or
binary change invalidates the chain; callers cannot make it pass by supplying
boolean fields. By default build requires current passing audit evidence and
transfer requires matching current audit and structured-review evidence;
`--allow-failed` is an explicit compatibility override, not proof.

Every operate session persists human-auditable artifacts under
`.ouvrier/operate/sessions/<id>/`: `transcript.jsonl`, `events.jsonl`,
`tool-calls.jsonl`, `auth_profile.json`, `goal.md`, `patch.json`,
`diff.patch`, `review.json`, `audit.json`, `build.json`, and `transfer.json`.
This keeps the worker runtime unchanged while giving the developer-operator
one local interface for construction and local verification. Existing transfer
and web-console surfaces are retained for compatibility and maintenance; they
are not active cockpit-development requirements.

## Build And Deploy

Deployment PaaS and web-console development are paused pending an explicit
redesign. The deployment material below documents already shipped compatibility
commands for operators who still use them; it is not an active product roadmap
or a completion requirement for the cockpit.

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

### Deploy Environments

Declare your servers once, committed, in pip.yaml — clone + ssh key + env
file is everything a teammate (or CI) needs to deploy:

```yaml
deploy:
  staging:
    hosts: [deploy@stg-1.example.com]
    port: 22                       # optional
    path: /opt/ouvrier/myworker    # optional
    service: ouvrier-myworker      # optional
    identity: ~/.ssh/ci_ed25519    # optional, for agent-less CI
  prod:
    hosts: [deploy@prod-1, deploy@prod-2]   # ~/.ssh/config aliases work too
```

Then:

```sh
ouvrier deploy staging
ouvrier deploy prod --yes        # prod/production requires --yes or a confirm
ouvrier deploy ssh --host deploy@stg-1.example.com   # one-off, same flow
```

Secrets ship from a per-environment dotenv at the project root —
`.env.staging`, `.env.prod`, falling back to `.env` (override with
`--env-file` or `OUVRIER_DEPLOY_ENV_FILE`). The preflight refuses git-tracked
env files and validates pip.yaml `env.required` plus `OUVRIER_ADMIN_TOKEN`,
reporting missing names only.

Each host deploy is an atomic release switch:

1. Build a static binary (`--target linux/arm64` for ARM hosts), sha256 it.
2. Remote preflight over one SSH session: passwordless-sudo probe, systemd
   check, create the dedicated `ouvrier-<name>` nologin user, create the
   layout under `<path>`, take the `.deploy.lock` flock.
3. Upload into the immutable `releases/<UTCts>-<gitsha>/` directory (binary,
   `RELEASE.json`, `skills/` assets), verify the remote sha256, chmod 0755.
4. Ship the env file atomically to `shared/.env` (root:service 0640 — the
   worker reads its secrets but cannot rewrite them). If the file does not
   set `OUVRIER_ADMIN_ADDR`, the deploy appends
   `OUVRIER_ADMIN_ADDR=127.0.0.1:9090` so the admin API stays
   loopback-only; a non-loopback value is refused without
   `--allow-shared-admin`.
5. Install the hardened systemd unit only when its content changed
   (+ `daemon-reload`), enable it. Disable the sandbox block with
   `--unit-sandbox off` or pip.yaml `deploy.<env>.sandbox: off`.
6. Atomically repoint the `current` symlink; `systemctl restart`.
7. Health gate: on-host curl of `127.0.0.1:<admin port>/admin/health`, 10
   attempts over ~30 seconds. The bearer token is fed to `curl -K -` as a
   stdin config — never in argv, never on disk, masked in all output.
8. On success: append `deploys.log`, prune to `--keep 5` releases, record
   the deploy in `~/.config/ouvrier/deployments.json`. On failure: dump
   `journalctl -u <service> -n 50`, repoint `current` to the recorded
   previous release, restart, re-run the health gate, and exit nonzero with
   both errors. A first deploy (nothing to roll back to) stops the service
   and reports.

Multi-host environments deploy sequentially, health-check each host, and
abort on the first failure with a loud mixed-version summary.

### Rollback

Instant rollback is the payoff of the releases/current layout:

```sh
ouvrier deploy rollback staging
ouvrier deploy rollback prod --yes     # same confirmation gate as deploy
```

Per host, `deploy rollback` takes the same `.deploy.lock`, reads the **last**
`deploys.log` entry, and repoints `current` at the release that entry
replaced — the ledger records the actual previous `current` target at deploy
time, so rollback never trusts timestamp ordering. It then restarts the
service, runs the same health gate, appends a distinguishable `rollback`
entry to `deploys.log`, and updates the deployments inventory (result
`rollback-ok`; the binary's sha256 is not recomputed — the entry records the
release ID now live). It refuses with an actionable error — leaving
`current` untouched — when there is no deploy history, when the last deploy
recorded no previous release (a first deploy), or when the previous release
directory was pruned by `--keep`; redeploy a known-good revision instead.
Multiple hosts roll back sequentially and abort on the first failure.

The host's `shared/.env` is intentionally **not** rolled back: the latest
shipped secrets stay in place (snapshotting the env per release is a pending
design decision). The local env file is read only for the
`OUVRIER_ADMIN_TOKEN` and `OUVRIER_ADMIN_ADDR` the health gate needs, so its
token must match the one already deployed.

### Server Preparation (sudoers)

The deploy logs in as an unprivileged user and uses `sudo -n` for exactly the
privileged commands the flow runs — full paths, fixed arguments, no
wildcards. Generate the matching least-privilege sudoers snippet:

```sh
ouvrier deploy ssh --print-sudoers --user deploy
```

which renders (for a worker named `myworker`):

```text
# Least-privilege sudoers for Ouvrier deploys of worker "myworker".
# Install as /etc/sudoers.d/ouvrier-myworker (mode 0440); validate with visudo -cf.
deploy ALL=(root) NOPASSWD: /usr/bin/true
deploy ALL=(root) NOPASSWD: /usr/sbin/useradd --system --home-dir /opt/ouvrier/myworker --no-create-home --shell /usr/sbin/nologin ouvrier-myworker
deploy ALL=(root) NOPASSWD: /usr/bin/install -d -m 0755 -o deploy -- /opt/ouvrier/myworker
deploy ALL=(root) NOPASSWD: /usr/bin/install -d -m 0750 -o root -g ouvrier-myworker -- /opt/ouvrier/myworker/shared
deploy ALL=(root) NOPASSWD: /usr/bin/install -d -m 0750 -o ouvrier-myworker -g ouvrier-myworker -- /opt/ouvrier/myworker/shared/state
deploy ALL=(root) NOPASSWD: /usr/bin/install -o root -g ouvrier-myworker -m 0640 -- /opt/ouvrier/myworker/.env.new /opt/ouvrier/myworker/shared/.env
deploy ALL=(root) NOPASSWD: /usr/bin/install -m 0644 -- /opt/ouvrier/myworker/ouvrier-myworker.service /etc/systemd/system/ouvrier-myworker.service
deploy ALL=(root) NOPASSWD: /usr/bin/systemctl daemon-reload
deploy ALL=(root) NOPASSWD: /usr/bin/systemctl enable ouvrier-myworker.service
deploy ALL=(root) NOPASSWD: /usr/bin/systemctl restart ouvrier-myworker.service
deploy ALL=(root) NOPASSWD: /usr/bin/systemctl stop ouvrier-myworker.service
deploy ALL=(root) NOPASSWD: /usr/bin/journalctl -u ouvrier-myworker.service -n 50 --no-pager
```

Install it once per worker on the server:

```sh
ouvrier deploy ssh --print-sudoers --user deploy | ssh root@stg-1.example.com \
  'cat > /etc/sudoers.d/ouvrier-myworker && chmod 0440 /etc/sudoers.d/ouvrier-myworker && visudo -cf /etc/sudoers.d/ouvrier-myworker'
```

### Deploying From CI (from scratch)

A fresh clone deploys with nothing but an SSH key and the env file. Pin the
host once, locally, and commit the result:

```sh
ouvrier server trust stg-1.example.com --fingerprint SHA256:f/+IMT34E8qsxk2X...
git add ouvrier.known_hosts && git commit -m "trust stg-1.example.com"
```

Then a complete GitHub Actions job:

```yaml
deploy-staging:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4          # brings pip.yaml + ouvrier.known_hosts
    - uses: actions/setup-go@v5
      with: { go-version: stable }
    - name: Install the ouvrier CLI
      run: go install github.com/ArnaudGuiovanna/ouvrier/cmd/ouvrier@latest
    - name: Install the deploy key and env file
      env:
        DEPLOY_KEY: ${{ secrets.STAGING_DEPLOY_KEY }}
        STAGING_ENV: ${{ secrets.STAGING_ENV_FILE }}   # full dotenv content
      run: |
        install -m 0600 /dev/null ci_ed25519 && printf '%s\n' "$DEPLOY_KEY" > ci_ed25519
        install -m 0600 /dev/null .env.staging && printf '%s\n' "$STAGING_ENV" > .env.staging
    - name: Deploy
      run: ouvrier deploy staging --identity ./ci_ed25519 --yes
```

The committed `ouvrier.known_hosts` makes the host-key decision part of code
review; `--fingerprint` keeps the initial pinning non-interactive; the deploy
itself needs no further confirmation for non-production environments.

Build a distroless container image instead:

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

If a provider stops because its output-token limit was reached, Ouvrier marks
the execution `truncated` and emits `budget_exceeded` with
`budget=provider_max_tokens`. Partial text remains observable, but partial tool
calls are never accepted or executed. HTTP pipelines return
`pipeline_execution_incomplete` instead of presenting that response as a
successful typed result.

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
