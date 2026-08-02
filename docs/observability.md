# Observability

Ouvrier exposes execution through three layers:

1. **EventStream** — logically append-only structured events for everything the
   harness does (pipeline, pipe, session, LLM call, tool call, schema
   validation, subagent task, hook invocation, permission decision). The
   in-process stream keeps a bounded recent window of 4,096 sanitized events;
   subscribers still receive every event in monotonic ID order, and events are
   persisted to `StateStore` independently of that memory window. All external
   output is redacted.
2. **Admin HTTP endpoints** — `/admin/health`, `/admin/status`,
   `/admin/plans`, `/admin/capabilities`, `/admin/events`,
   `/admin/traces?last=N`, `/admin/traces/<exec-id>`, `POST /admin/trigger`,
   `GET /admin/approvals`, `POST /admin/approvals/<id>`,
   `POST /admin/streams/replay`, and (dev-mode only) `GET /dev` for the trace
   viewer UI.
3. **Tracer hooks** — an OTel-compatible `Tracer` interface that pairs
   `*_started` and `*_completed` / `*_failed` events into spans.

## Cockpit session journals

The local `ouvrier operate` cockpit has its own per-session append-only files
under `.ouvrier/operate/sessions/<id>/`; these are distinct from the worker
runtime `EventStream` above. `transcript.jsonl` accepts records up to 8 MiB and
is capped at 64 MiB; transcript readers and export reject more than 100,000
entries. `/compact` appends a deterministic context checkpoint instead of
rewriting this history, and the Markdown export is atomically capped at
128 MiB.

`events.jsonl` accepts individual events up to 4 MiB. Resume and replay reject
an event journal over 64 MiB or 100,000 entries. All event fields are redacted
before persistence, and an append error propagates to the agent turn so it
cannot report success after losing its audit trail. `Subscribe` validates the
whole journal before returning a stream and never emits a partial replay for a
corrupt or oversized file.

With the session writer lock held, resume may add a missing newline to a valid
last JSONL record or discard only an invalid unterminated final fragment. A
discard is made observable by a status entry in `transcript.jsonl`. Corruption
in a complete or middle record remains an error and is never silently repaired.
Patch and audit coordinator errors are likewise observable in `session.json`:
their durable states are `patch_failed` and `audit_failed`, with a redacted
`last_error` and transition reason. A completed audit whose gates report
failure also transitions to `audit_failed`; `audit.json` and the transition
reason remain the evidence when there is no execution error to store in
`last_error`.

## Admin endpoints

All admin endpoints require `Authorization: Bearer <OUVRIER_ADMIN_TOKEN>` outside
local development. Responses are JSON and redact secrets before they leave the
process.

| Endpoint                       | Use                                           |
| ------------------------------ | --------------------------------------------- |
| `GET /admin/health`            | Liveness probe.                               |
| `GET /admin/status`            | Counters derived from EventStream + StateStore: sessions, executions by status, LLM usage, harness metrics, schema violations, budget usage. |
| `GET /admin/plans`             | Machine-readable compiled triggers, steps, tools, schemas, and terminals. |
| `GET /admin/capabilities`      | Integration-oriented alias for compiled worker capabilities. |
| `GET /admin/events`            | Redacted event stream as JSONL by default or SSE with `?format=sse`; add `?follow=false` for a snapshot. |
| `GET /admin/traces?last=N`     | Recent execution headers.                     |
| `GET /admin/traces/<exec-id>`  | Full event timeline for one execution.        |
| `POST /admin/trigger`          | Manually fire a trigger (cron/HTTP/stream) and return `exec_id`, `trace_id`, and `session_id` when scheduled. |
| `GET /admin/approvals`         | Pending human-in-the-loop approvals.          |
| `POST /admin/approvals/<id>`   | Approve or deny a suspended tool call.        |
| `POST /admin/streams/replay`   | Replay the runtime-retained DLQ copy for one configured stream plan. |
| `GET /metrics`                 | Prometheus text exposition (counters + latency summaries). |
| `GET /dev`                     | Local dev-mode trace viewer UI.               |

Consume them from the terminal with the CLI:

```sh
ouvrier status                        # /admin/status
ouvrier logs --last 50                # /admin/traces?last=50
ouvrier trace <exec-id>               # /admin/traces/<exec-id>
```

`OUVRIER_ADMIN_TOKEN` from the environment is used automatically.

## Dedicated admin listener (`OUVRIER_ADMIN_ADDR`)

By default the admin surface shares the public port, exactly as in v0.2. Set
`OUVRIER_ADMIN_ADDR` (e.g. `127.0.0.1:9090`) and `Run` starts a second
listener with its own mux: every `/admin/*` route, `GET /metrics`, and the
dev-mode `GET /dev` viewer answer only on that listener, and the public port
returns 404 for them. Trigger routes are unaffected. This is how the v0.3
constraint "admin ports are never exposed publicly" is enforced — operators
reach the admin listener over an SSH tunnel rather than the network.

- The bind must be loopback. A non-loopback `OUVRIER_ADMIN_ADDR` refuses
  startup regardless of token or dev mode, unless `OUVRIER_ADMIN_INSECURE=1`
  explicitly overrides it (a startup warning is logged in that case).
- Admin token enforcement is identical on the dedicated listener: bearer auth
  outside `OUVRIER_ENV=dev`, exactly as on the shared port.
- Cron- and stream-only workers, whose HTTP surface is admin-only, also move
  it to the admin listener; their public port then answers 404 for everything
  while still being bound by the worker.
- `ovr.Handler` (the in-process test seam) is unaffected: it always returns
  the combined handler with trigger and admin routes together, regardless of
  `OUVRIER_ADMIN_ADDR`. Only `Run` splits listeners.

### `OUVRIER_METRICS_PUBLIC`

When the surface is split, `/metrics` moves to the admin listener with the
rest. For Prometheus scrapers that cannot traverse SSH to the loopback admin
port, set `OUVRIER_METRICS_PUBLIC=1` to keep `/metrics` *also* registered on
the public port (default when split: not public). The public copy enforces the
same bearer-token auth. The variable changes nothing while
`OUVRIER_ADMIN_ADDR` is unset.

## Prometheus `/metrics`

`GET /metrics` returns a hand-rolled Prometheus text exposition (format version
0.0.4, no third-party dependency) computed on demand from the EventStream and
StateStore. It shares the admin auth posture: bearer-token protected outside
local dev mode (`OUVRIER_ENV=dev`), exactly like the `/admin/*` endpoints.

Counters (all monotonic `_total`):

```
ouvrier_pipeline_started_total / _completed_total / _failed_total
ouvrier_pipe_started_total     / _completed_total / _failed_total
ouvrier_llm_call_started_total / _completed_total / _failed_total
ouvrier_tool_call_started_total / _completed_total / _failed_total
```

Latency summaries (emitted as `_sum` and `_count` series):

```
ouvrier_llm_call_duration_ms     # from sanitized llm_call_completed latency_ms
ouvrier_pipeline_duration_ms     # paired pipeline_started -> completed/failed
ouvrier_pipe_duration_ms         # paired pipe_started -> completed/failed
ouvrier_tool_call_duration_ms    # paired tool_call_started -> completed/failed
```

Values are derived purely from canonical event kinds and the already-sanitized
`latency_ms` payload, plus event timestamps for the paired durations, so no raw
payload content (and therefore no secrets) ever reaches the exposition.
The `_total`, `_sum`, and `_count` values use fixed-cardinality lifetime
aggregates, so evicting old events from the in-process recent window cannot make
a metric decrease during the process lifetime. Durable `StateStore` history is
merged when available and remains the source across restarts.

Scrape it with Prometheus:

```yaml
scrape_configs:
  - job_name: ouvrier
    metrics_path: /metrics
    authorization:
      credentials: <OUVRIER_ADMIN_TOKEN>
    static_configs:
      - targets: ["ouvrier:8080"]
```

With `OUVRIER_ADMIN_ADDR` set, `/metrics` lives on the admin listener; point
the scraper there (loopback/tunnel) or set `OUVRIER_METRICS_PUBLIC=1` to keep
it on the public port as well.

## Native OTLP exporter

`ovr.WithOTLPExporter(endpoint, opts...)` installs a built-in `Tracer` that
ships spans to an OTLP/HTTP collector. It is hand-rolled (no OpenTelemetry SDK):
spans are encoded as OTLP/HTTP JSON and POSTed to `<endpoint>/v1/traces` behind
an injectable HTTP client. Default off — unset means no behavior change.

```go
runner := ovr.NewRunner(
    ovr.WithOTLPExporter("https://collector:4318",
        ovr.OTLPServiceName("orders-api"),
        ovr.OTLPHeaders(map[string]string{"Authorization": "Bearer <token>"}),
    ),
)
```

The exporter reuses the same span-pairing logic as the `Tracer` hook (one span
per pipeline, pipe, session, LLM call, tool call, schema validation, subagent
task) and redacts span attributes before export (sensitive keys and
credential-looking strings). Export failures are swallowed so a flaky collector
never breaks pipeline execution. `WithOTLPExporter` is a convenience wrapper over
`WithTracer`; passing both means the last option wins.

## OpenTelemetry / custom tracers

Install a `Tracer` on the runner:

```go
import (
    "context"

    ovr "github.com/ArnaudGuiovanna/ouvrier"
)

type myTracer struct{ /* wraps your tracing stack */ }

func (m *myTracer) StartSpan(ctx context.Context, name string, attrs map[string]any) (context.Context, ovr.Span) {
    // hand off to OTel, Jaeger, Honeycomb, ...
    span := newSpan(name, attrs)
    return ctx, span
}

func main() {
    runner := ovr.NewRunner(ovr.WithTracer(&myTracer{}))
    _ = runner.Run(":8080",
        ovr.From("POST /tickets"),
        ovr.Pipe("...", ovr.Model("anthropic/claude-sonnet-4-6")),
        ovr.Reply(ovr.JSON[map[string]any]()),
    )
}
```

`Span` requires three methods:

```go
type Span interface {
    End()
    SetAttribute(key string, value any)
    RecordError(err error)
}
```

Ouvrier emits one span per:

| Span name           | Pair                                                  |
| ------------------- | ----------------------------------------------------- |
| `pipeline`          | `pipeline_started` → `pipeline_completed` / `pipeline_failed` |
| `pipe`              | `pipe_started` → `pipe_completed` / `pipe_failed`     |
| `session`           | `session_started` → `session_saved` / `session_cancelled` |
| `llm_call`          | `llm_call_started` → `llm_call_completed` / `llm_call_failed` |
| `tool_call`         | `tool_call_started` → `tool_call_completed` / `tool_call_failed` |
| `schema_validation` | `schema_validation_passed` / `schema_validation_failed` |
| `subagent_task`     | `task_started` → `task_completed` / `task_failed`     |

Span attributes include the event's `exec_id`, `session_id`, `trace_id`, and
any sanitized payload fields (model, tokens, duration, error). Sensitive keys
(`authorization`, `*token`, `*secret`, `*password`, `api_key`, cookies) are
redacted before they reach the tracer.

`ovr.NopTracer()` returns a tracer whose spans do nothing, which is useful for
tests or when you want the option present without an external observability
stack.

## Trace correlation

EventStream events carry `ExecID`, `SessionID`, and `TraceID` so external
observability stacks can correlate Ouvrier executions with upstream and
downstream systems. `Tracer` adapters typically map `TraceID` to OTel's
`trace.SpanContext.TraceID` and treat `ExecID` as a span attribute.

## Limitations

- **Cost estimates are best-effort.** Provider metadata exposes tokens and
  latency; cost is attached when a pricing table matches the model and the
  provider exposes usable token metadata.
- **DLQ replay drains Ouvrier's retained copy.** Broker DLQ targets are
  published over the real transport, but `/admin/streams/replay` replays the
  runtime-retained DLQ copy for the configured plan. Replaying arbitrary broker
  DLQ topics or streams requires a dedicated consumer/custom DLQ integration.
- **Trace persistence** uses the same `StateStore` as everything else
  (SQLite by default). For high-volume production workloads, configure a
  longer-retention backend by implementing `state.Store`.
- **The in-process EventStream is a recent-event cache.** Admin trace and event
  APIs read the durable `StateStore` first and therefore are not truncated by
  the 4,096-event memory window. A deliberately store-less internal harness can
  expose only that retained window through its fallback admin surface.
