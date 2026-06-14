# Ouvrier API Reference

## Mental Model

An Ouvrier worker has four layers:

1. **Trigger** — `ovr.From(...)` declares what starts the pipeline: an HTTP request, a cron schedule, a signed webhook, or a stream message.
2. **Pipeline** — `ovr.Pipe(goal, opts...)` is one agent step. It receives the trigger payload, reasons toward the goal, and can call governed tools.
3. **Governed tools** — `ovr.Tool(name, fn, opts...)` exposes Go functions to the agent under an explicit safety policy (ReadOnly, SideEffecting, Idempotent, RequiresApproval).
4. **Reply / Outcome** — `ovr.Reply(ovr.JSON[T]())` answers an HTTP caller; `ovr.Sink(ovr.Log())` or `ovr.Push(target)` handles non-HTTP workers.

`ovr.Run(addr, nodes...)` assembles and starts the worker, binding HTTP/cron/webhook/stream sources to their pipelines.

## Primitives

- `ovr.Run(addr, nodes...)` — start an HTTP/cron/webhook/stream worker on the given address.
- `ovr.From("METHOD /path")` — declare an HTTP trigger (GET or POST).
- `ovr.From(ovr.Cron("0 6 * * *"))` — declare a cron schedule trigger.
- `ovr.From(ovr.Webhook("github"))` — declare a signed webhook trigger.
- `ovr.From(ovr.Stream("kafka://broker/topic"))` — declare a stream trigger (kafka/redis/nats).
- `ovr.Pipe(goal, opts...)` — declare one agent pipeline step with a goal, model, tools, and typed output.
- `ovr.Model("anthropic/claude-sonnet-4-6")` — select the LLM model for a Pipe (provider/model form required).
- `ovr.Output[T]()` — declare the typed structured result schema expected from a Pipe.
- `ovr.Tool(name, fn, opts...)` — register a Go function as an agent tool for a Pipe.
- `ovr.ReadOnly()` — mark a tool as side-effect free; eligible for safe retry and parallel execution.
- `ovr.SideEffecting(label)` — mark a tool that mutates external state (label required).
- `ovr.Idempotent(keyExpr)` — mark a side-effecting tool as replay-safe under a stable idempotency key expression.
- `ovr.RequiresApproval()` — require explicit human approval before the tool runs.
- `ovr.Describe("what it does")` — set the LLM-facing description for a Tool.
- `ovr.Reply(format)` — terminate a synchronous pipeline by answering the HTTP caller.
- `ovr.JSON[T]()` — typed JSON reply format for Reply.
- `ovr.Accepted()` — HTTP 202 reply format; pipeline continues asynchronously.
- `ovr.Sink(target)` — terminate a pipeline without replying to the trigger source.
- `ovr.Log()` — log sink target; writes outcome to logs and metrics.
- `ovr.Push(target)` — terminate an asynchronous pipeline by sending the outcome to a webhook or queue.
- `ovr.Webhook("https://...")` — webhook push target for Push.
- `ovr.Queue("kafka://broker/topic")` — queue push target for Push.
- `ovr.SubAgent(name, pipeline, opts...)` — expose a child Pipeline as a governed tool for a Pipe.
- `ovr.Pipeline(nodes...)` — declare a child pipeline for SubAgent.
- `ovr.Fallback(models...)` — ordered fallback model list for a Pipe on transient provider failure.
- `ovr.Retry(max, policy)` — configure transient provider retries for a Pipe.
- `ovr.Timeout("30s")` — configure wall-clock budget for a Pipe.

## Governance Patterns

```go
// Pure read — safe to retry and parallelise
ovr.Tool("search", searchFn, ovr.ReadOnly(), ovr.Describe("search the index"))

// Side-effecting — labelled mutation
ovr.Tool("send-email", sendFn, ovr.SideEffecting("email"), ovr.Describe("send an email"))

// Idempotent — safe to replay under a stable key
ovr.Tool("charge", chargeFn, ovr.Idempotent("{{.OrderID}}"), ovr.Describe("charge the card"))

// Risky — blocked until a human approves
ovr.Tool("delete-db", deleteFn, ovr.RequiresApproval(), ovr.Describe("drop the database"))
```

## Worker Skeleton

```go
package main

import (
    "context"
    ovr "github.com/ArnaudGuiovanna/ouvrier"
)

type Result struct {
    Answer string `json:"answer"`
}

func main() {
    ovr.Run(":8080",
        ovr.From("POST /ask"),
        ovr.Pipe("answer the question",
            ovr.Model("anthropic/claude-sonnet-4-6"),
            ovr.Tool("lookup", lookupFn, ovr.ReadOnly(), ovr.Describe("look up a fact")),
            ovr.Output[Result](),
        ),
        ovr.Reply(ovr.JSON[Result]()),
    )
}
```
