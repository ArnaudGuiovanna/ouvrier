# Observability

Ouvrier exposes execution through three layers:

1. **EventStream** — append-only structured events for everything the harness
   does (pipeline, pipe, session, LLM call, tool call, schema validation,
   subagent task, hook invocation, permission decision). Events are persisted
   to `StateStore` and redacted before any external output.
2. **Admin HTTP endpoints** — `/admin/health`, `/admin/status`,
   `/admin/traces?last=N`, `/admin/traces/<exec-id>`, `POST /admin/trigger`,
   and (dev-mode only) `GET /dev` for the trace viewer UI.
3. **Tracer hooks** — an OTel-compatible `Tracer` interface that pairs
   `*_started` and `*_completed` / `*_failed` events into spans.

## Admin endpoints

All admin endpoints require `Authorization: Bearer <PIP_ADMIN_TOKEN>` outside
local development. Responses are JSON and redact secrets before they leave the
process.

| Endpoint                       | Use                                           |
| ------------------------------ | --------------------------------------------- |
| `GET /admin/health`            | Liveness probe.                               |
| `GET /admin/status`            | Counters derived from EventStream + StateStore: sessions, executions by status, LLM usage, harness metrics, schema violations, budget usage. |
| `GET /admin/traces?last=N`     | Recent execution headers.                     |
| `GET /admin/traces/<exec-id>`  | Full event timeline for one execution.        |
| `POST /admin/trigger`          | Manually fire a trigger (cron/HTTP/stream).    |
| `GET /metrics`                 | Prometheus text exposition (counters + latency summaries). |
| `GET /dev`                     | Local dev-mode trace viewer UI.               |

Consume them from the terminal with the CLI:

```sh
ouvrier status                        # /admin/status
ouvrier logs --last 50                # /admin/traces?last=50
ouvrier trace <exec-id>               # /admin/traces/<exec-id>
```

`PIP_ADMIN_TOKEN` from the environment is used automatically.

## Prometheus `/metrics`

`GET /metrics` returns a hand-rolled Prometheus text exposition (format version
0.0.4, no third-party dependency) computed on demand from the EventStream and
StateStore. It shares the admin auth posture: bearer-token protected outside
local dev mode (`PIP_ENV=dev`), exactly like the `/admin/*` endpoints.

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

Scrape it with Prometheus:

```yaml
scrape_configs:
  - job_name: ouvrier
    metrics_path: /metrics
    authorization:
      credentials: <PIP_ADMIN_TOKEN>
    static_configs:
      - targets: ["ouvrier:8080"]
```

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

## Limitations (v0.1)

- **Cost estimates are best-effort.** Provider metadata exposes tokens and
  latency; cost is only attached when the provider returns a cost figure (none
  currently do for v0.1 prefixes), so dashboards should compute cost from
  token usage and your own pricing table.
- **Provider streaming events** are not surfaced through Ouvrier's EventStream
  in v0.1; only request lifecycle (`llm_call_started` / `llm_call_completed`)
  is observable. `Reply(SSE())` streams the final reply to the HTTP client but
  is not driven by upstream provider streaming.
- **Trace persistence** uses the same `StateStore` as everything else
  (SQLite by default). For high-volume production workloads, configure a
  longer-retention backend by implementing `state.Store`.
