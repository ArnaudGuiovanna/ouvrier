# Google ADK Go v2 cockpit spike

Status: **mechanics GO; production integration remains NO-GO**.

This isolated module tests Google ADK Go v2 as an orchestration engine for the
fully Go Ouvrier worker-building cockpit. It is not linked from the root module
and changes neither the worker runtime nor the paused PaaS and web console.

## Dependency decision

The spike pins `google.golang.org/adk/v2 v2.0.0` because it supports the
project's Go 1.25 toolchain. ADK v2.1.0 was also inspected, but its module
requires Go 1.26.5. A toolchain upgrade is a separate decision.

ADK v2.0.0 is Apache-2.0, but its module graph is broad and includes Google
Cloud, gRPC and OpenTelemetry dependencies. It therefore remains in this
nested module and must not enter Ouvrier's root `go.mod`.

Official sources:

- <https://github.com/google/adk-go>
- <https://pkg.go.dev/google.golang.org/adk/v2@v2.0.0>
- <https://pkg.go.dev/google.golang.org/adk/v2@v2.0.0/agent/workflowagents/loopagent>
- <https://github.com/google/adk-go/releases/tag/v2.1.0>
- <https://raw.githubusercontent.com/google/adk-go/v2.1.0/go.mod>

## Closed product surface

`NewRepairKernel` is the only product constructor in the spike. Its
`RepairConfig` accepts:

- one ADK model;
- declarative Ouvrier `ToolSpec` values;
- one Ouvrier-owned `GovernedExecutor`;
- a mandatory non-zero `MaxIterations`.

The constructor builds the `llmagent` stage, governed function-tool adapters
and bounded `LoopAgent` internally. No caller can inject an arbitrary
`agent.Agent`, workflow, session service or raw ADK tool. Every declared tool
is wired to the same governed executor, and at least one tool must be declared
as completion evidence.

```text
NewRepairKernel(model + ToolSpec + MaxIterations)
        |
internal ADK runner / llmagent / LoopAgent
        |
internal function-tool adapter
        |
Ouvrier GovernedExecutor
        |
permission + sandbox + redaction + transcript + audit + operation
```

The existing `operate.ToolRegistry.Execute` is not yet a safe production
implementation of `GovernedExecutor`: approval, transcript persistence and
tool-call audit are currently coordinated around it by
`AgentRuntime.callTool`. Those responsibilities must first be extracted into
one executor that cannot be bypassed.

The function-tool input schema is deliberately narrow (`attempt`, `goal` and
`query`). It proves the execution boundary, not the complete Ouvrier tool
catalog. A later adapter must preserve each native tool's real JSON Schema.

## Completion and outcomes

Every live run ends with exactly one explicit `EventOutcome`:

- `verified`: a completion tool crossed `GovernedExecutor` and returned
  verified evidence;
- `exhausted`: the bounded workflow ended without that evidence;
- `cancelled`: the context was cancelled or its deadline elapsed;
- `failed`: the runner, model, tool adapter or event mapper failed.

ADK model text is always normalized as assistant text, even when ADK calls it a
final response. It never becomes Ouvrier `EventFinal`. A verified final is
emitted only when the function response correlates with a completion-tool call
recorded by the internal proof tracker under
`(InvocationID, FunctionCallID, ToolName)`.

The tracker is live-only, consumes each proof once and is purged at the end of
the invocation. It writes no provenance token into model-visible content. This
is sufficient to reject forged or replayed function responses during the
spike, but it is not a durable proof journal.

Tool arguments, executor results and normalized event maps are deep-copied
through JSON before crossing their respective boundary. Non-JSON values fail
closed rather than sharing mutable maps between ADK, Ouvrier and clients.

## Session and replay limits

Session IDs are trimmed consistently and empty IDs are rejected.
`session.InMemoryService` remains the only backend in this module.

`ReplayPersisted` is deliberately narrower than the live stream:

- live SSE partials are exposed as `assistant_delta`, but partials are not
  promised by persisted replay;
- synthetic outcome events are live-only;
- replay does not recertify a historical function response as `EventFinal`,
  because the live proof tracker has already consumed the evidence;
- each part of a multi-part ADK event gets a stable
  `<adk-event-id>/part/<index>` ID, and the live verified derivative gets a
  distinct `/verified` suffix;
- when ADK supplies no event ID, the invocation/author fallback is
  best-effort and is not claimed globally unique.

Durable resume, durable verified outcomes and lossless delta replay therefore
remain explicit production NO-GO items.

## Hermetic acceptance tests

Run the nested module explicitly:

```bash
cd spikes/adk-cockpit
GOWORK=off go test ./...
GOWORK=off go test -race ./...
GOWORK=off go vet ./...
```

The in-memory tests prove:

1. The exported constructor rejects open, ungoverned and unbounded
   configurations.
2. Runner/model/tool events are normalized in order without promoting model
   text to verified completion.
3. `LoopAgent` stops at the configured maximum and reports `exhausted`.
4. It exits early with `verified` only after governed completion evidence.
5. A verified result from a non-completion tool cannot finish the workflow.
6. Cancellation and provider failures produce explicit terminal outcomes.
7. Empty session IDs are rejected and surrounding whitespace is resolved
   consistently.
8. Multi-part event IDs are stable and unique within the ADK event, and JSON
   maps are detached before execution and yield.
9. A forged response cannot become final, and a governed proof cannot be
   reused.
10. Live deltas are observed but are not falsely advertised as replayable.

## Go / no-go criteria

### GO for a second isolated integration slice

- In-process runner, model, session, event and function-tool mechanics work.
- A bounded `LoopAgent` represents an Ouvrier repair cycle.
- The product surface can stay closed around Ouvrier configuration.
- Governed live evidence can be distinguished from model-declared success.
- Nested cancellation propagates through the tested workflow.
- The ADK dependency remains isolated from the framework runtime.

### NO-GO for production integration until all are resolved

- Extract a production `GovernedExecutor` that cannot bypass permissions,
  sandboxing, redaction, transcript persistence or audit.
- Add an Ouvrier-owned durable proof journal and prove process-restart resume;
  ADK in-memory history is not authoritative evidence.
- Complete the event mapper for usage, errors, workflow branches, approvals
  and redaction, and specify the desired persistence semantics for deltas.
- Adapt Ouvrier's provider-neutral model interface without losing parallel
  function calls, call IDs, usage or cancellation.
- Resolve the Codex transport gap: current `codex exec` integration returns
  text and does not expose structured Ouvrier tool calls.
- Derive real tool JSON Schemas instead of the spike's placeholder arguments.
- Measure binary size, cold start, vulnerabilities and upgrade impact before
  choosing the final module or binary boundary.

The next slice should remain read-only: expose worker-file reads, Ouvrier
documentation search and deterministic audit through the extracted governed
executor, then design durable proof persistence before enabling writes.
