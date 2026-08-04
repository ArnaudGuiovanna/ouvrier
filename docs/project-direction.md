# Ouvrier Project Direction

**Decision date:** 2026-07-26

**Implementation status update:** 2026-08-03

**Status:** active project memory

**Scope:** priority order and product boundaries for work after `v0.5.5`

This document records the current product direction. It takes precedence over
older milestone specs when their priorities or acceptance criteria conflict
with it. Historical specs remain useful descriptions of shipped code and past
intent, but they are not an instruction to continue every workstream.

## Parallel Priority Workstreams

1. **Priority 0A — stabilize the Ouvrier worker runtime framework.**
2. **Priority 0B — build the specialized Ouvrier cockpit harness in parallel.**
3. **Paused — revisit deployment platform and web-console design later.**

Tracks 0A and 0B are both active and may progress concurrently. Reducing the
supported runtime perimeter is acceptable when that is the clearest path to a
reliable, robust release. No cockpit, IDE, deployment, or console requirement
may force an unstable runtime contract into the stabilization milestone.

## First Parallel Slices

The first implementation slices started on 2026-07-26:

- **0A.1 — provider truncation is fail-closed.** A provider response stopped by
  its output-token limit is `truncated`, including during schema repair.
  Partial tool calls are observable but are never accepted or executed, and
  terminal events and state persistence survive caller cancellation.
- **0B.1 — isolated ADK mechanics spike.** The nested
  `spikes/adk-cockpit` Go module evaluates runner, session, event, governed-tool,
  cancellation, and bounded-loop mechanics without adding ADK to the runtime
  module graph. Its own CI job runs module verification, vet, tests, and race
  tests. Passing mechanics authorize only the next read-only cockpit slice;
  they do not authorize production adoption.

Those initial slices have expanded into the stabilization baseline below. The
full runtime, cockpit, race, static-analysis, build, and generated-worker gates
now pass for the candidate carried by both `main` and `staging`. Passing those
repository gates establishes a release candidate; implementation on `main` is
not itself a release declaration. Independent QA acceptance, the final product
owner recipe, release documentation, and a version tag remain required.

## Current Stabilization Baseline (Repository Gates Passed)

As of 2026-08-03, the following contracts are implemented on `main` and have
passed the repository-wide verification gates on both active branches. They
define the current release candidate. The latest tagged release remains
`v0.5.5` until the candidate completes independent acceptance and receives a
new version tag.

- **Headless governance is fail-closed.** Prompt, print, JSON, and RPC turns
  start in the `manual` posture. Without an attached operator, read-only and
  idempotent tools may run, while side-effecting and `requires_approval` tools
  are denied and audited. `--auto-safe` is the explicit opt-in for
  side-effecting headless tools and never overrides `requires_approval`.
- **Coding-agent selection is explicit and real.** Interactive `--agent auto`
  detects Codex and Claude, reports saved-session/adapter readiness, and opens a
  Bubble Tea chooser; headless auto prefers ready Codex and then ready Claude.
  `codex`, `claude`, and `manual` remain explicit choices, and `ouvrier agents`
  exposes readiness without reading credentials. Both coding agents run through
  their canonical ACP v1 stdio adapters by default. Each receives bounded
  redacted source context and no agent-side tools, then returns a strict
  full-file patch plan that Ouvrier applies to a disposable staged Git copy.
  Native Codex App Server and legacy `exec` remain explicit compatibility/debug
  modes. Agent CLIs retain ownership of initial authentication and token refresh;
  the normal Ouvrier onboarding reuses their saved sessions and has no login command.
- **Candidate-executing audit gates require real isolation.** Production audit
  runs `go test`, `go vet`, and the static Linux/amd64 build in a disposable
  Linux Bubblewrap sandbox. Network, ambient workspaces, proxies, inherited
  credentials, and writable candidate source are removed after offline
  dependency preparation. Missing Linux/Bubblewrap/namespace guarantees fail
  closed; there is no host fallback for these gates.
- **Completion evidence is source-bound.** The source fingerprint covers the
  worker tree, relevant local `go.mod` replacement inputs, the Go toolchain,
  and `GOWORK=off`. Audit and strict structured-review evidence must match the
  current fingerprint. A trusted build must match it, bind the exact persisted
  passing audit by SHA-256, and record the compiled binary's SHA-256. Mutation
  or evidence tampering makes earlier proof stale.
- **Worker file tools have a bounded, sensitive-data-denying surface.** Model
  reads and writes have output/input caps; listing and literal search are
  paginated with traversal, byte, file, query, and result limits; removal is
  one file or one internal symlink. `.git`, `.ouvrier`, secret dotenv files,
  private keys, credential stores, external symlinks, and sensitive symlink
  targets are refused. `.env.example` remains readable as documentation.
- **Durable context is append-only and bounded.** `/compact` persists a real
  model-context checkpoint while preserving the full transcript for audit and
  export. Post-checkpoint model history, transcript records/files, event
  records/replay, exports, model steps, tool-call batches, and tool-result
  transport all have explicit fail-closed ceilings. Torn-tail recovery may
  repair only the final interrupted JSONL record; middle corruption is never
  rewritten.
- **Provider and coding-agent output is resource-bounded.** HTTP JSON bodies,
  SSE streams/frames/text/tool calls, Codex exec stdout/lines/text/stderr, App
  Server messages/text/stderr, and ACP lines/aggregate/text/stderr have tested
  byte/count ceilings. Overflow cancels or fails the operation, and bounded-loop
  exhaustion cannot become a successful completion.
- **Operator shell and repository inspection stay governed.** The model cannot
  call the shell. Each interactive `!`/`!!` command requires approval and runs
  only in a Linux Bubblewrap workspace with fixed environment, no network, a
  two-minute deadline, and bounded output. Read-only Git disables executable
  extension points and rejects all `filter=` attributes, including Git LFS;
  secret scans and complete review transports fail rather than silently
  truncating their bounded coverage.
- **Failure state is durable.** Patch and audit execution/evidence failures
  transition sessions to `patch_failed` or `audit_failed` and persist a
  redacted `last_error`, so interrupted evidence work is not represented by an
  in-progress or successful status.

The shared generated-worker lane remains the narrow integration proof: a
cockpit-created worker must compile against the current public `ovr` contract,
pass runtime checks, pass the isolated deterministic audit, and yield a local
artifact whose evidence can be independently recomputed.

Before a release tag is created, the final acceptance recipe must also prove
the human journey: construct or edit a worker through governed cockpit tools,
inspect its isolated audit, build the source-bound artifact, verify its local
health endpoint and checksum, close the cockpit process, resume the durable
session in a distinct process, and complete a follow-up turn without losing or
duplicating the earlier tool-call history. The executable repository gates and
manual commands are documented in the README and handbook.

## Priority 0A: Runtime Stabilization

Track 0A covers the framework runtime used by Ouvrier workers: trigger, goal,
tools, governed execution, and outcome.

The stabilization milestone must concentrate on:

- a small and explicit supported contract;
- deterministic validation and failure behavior;
- cancellation, timeouts, retries, idempotency, and recovery;
- permission enforcement and sandbox fail-closed behavior;
- schema correctness and typed output validation;
- provider and streaming behavior covered by contract tests;
- secret redaction on every observable path;
- race safety and bounded resource use;
- repeatable conformance, integration, race, and failure-injection tests.

The cockpit coding harness and the worker runtime harness are separate systems.
They may share design principles and internal utilities, but neither should
silently define the other's public API.

## Priority 0B: A Pi-Class Ouvrier Coding Harness

The target cockpit is a **fully Go implementation of a coding-agent harness
specialized in constructing Ouvrier workers**.

The useful mental model is:

> Codex, specialized end to end in the Ouvrier framework and limited to the
> construction, diagnosis, review, repair, and local validation of Ouvrier
> workers.

Pi is the architectural reference, not only a visual reference. Ouvrier should
transpose the useful harness architecture into Go:

- one streaming agent kernel with several clients such as TUI, print, JSON, and
  RPC;
- a real model/tool loop in which tool calls and results remain part of model
  history;
- an append-only event stream used for rendering, persistence, replay, and
  audit;
- durable sessions with resume, interruption, compaction, checkpoint, fork,
  and export;
- provider/model adapters behind stable Ouvrier-owned interfaces;
- first-class tools, skills, prompt packs, context injection, and structured
  tool results;
- deterministic permission gates before workspace writes, commands, network,
  secrets, or other privileged actions;
- bounded child tasks and workflow composition;
- visible plans, tool cards, diffs, findings, gate results, and artifacts;
- a terminal UI that is a client of the harness, never a second agent engine.

The specialization is binding. The cockpit is not a general-purpose repository
agent. Its tools, context, evaluation, and completion criteria are centered on:

- discovering or scaffolding an Ouvrier worker;
- using only real public `ovr` APIs for the framework version embedded in the
  CLI;
- editing ordinary, readable Go;
- selecting triggers, pipes, tools, outputs, governance, and schemas correctly;
- compiling and testing the worker;
- running deterministic Ouvrier audits;
- reviewing and repairing findings in a bounded loop;
- producing a local build artifact with provenance and checksum.

A model response is never proof of completion. A construction or repair loop
finishes only on observed evidence such as the resulting files and diff,
successful compilation/tests, passing required audits, and an identified
artifact.

### Full-Go Boundary

The cockpit kernel, workflow orchestration, tools, session/event persistence,
policy enforcement, TUI, and IDE must be implemented in Go.

- Do not introduce a TypeScript, Node.js, or Python harness runtime.
- Bubble Tea remains the terminal UI foundation.
- Generated workers remain ordinary Go projects.
- External model services and optional official provider CLIs may be reached
  through narrow adapters, but they do not own the Ouvrier agent loop, tool
  policy, state, or completion semantics.

## Google ADK Go v2: Candidate Foundation

Google ADK Go v2 (`google.golang.org/adk/v2`) is the preferred candidate to
evaluate for the cockpit's in-process agent and workflow foundation. As checked
on 2026-07-26, its public Go surface includes agents, an LLM agent, runners,
sessions, tools, events, and deterministic sequential, parallel, and loop
workflow agents:

- <https://github.com/google/adk-go>
- <https://pkg.go.dev/google.golang.org/adk/v2>
- <https://pkg.go.dev/google.golang.org/adk/v2/agent/workflowagents/loopagent>

Expected uses include:

- `SequentialAgent`-style flows for plan, edit, validate, and review stages;
- `ParallelAgent`-style flows for independent read-only inspections;
- `LoopAgent`-style bounded repair cycles such as
  `edit -> compile/test/audit -> critique -> repair`;
- runner/session/event primitives for streaming execution and resumability;
- function tools and agent-as-tool composition for Ouvrier-native operations.

ADK is a candidate implementation substrate, not the product architecture and
not an automatic dependency. Before adoption, implement a focused spike and
record a decision proving:

1. model/provider neutrality, including the intended Codex path;
2. lossless streaming of model, tool, and workflow events;
3. cancellation and interruption through nested and looped agents;
4. bounded loop termination by success condition, budget, and maximum
   iterations;
5. durable session resume without losing tool-call history;
6. compatibility with Ouvrier's deterministic permission gate, sandbox,
   redaction, audit, and artifact contracts;
7. clean dependency size, build, license, and upgrade behavior.

Ouvrier remains authoritative at security and side-effect boundaries. No ADK
tool or sub-agent may bypass Ouvrier's tool executor, permission policy,
sandbox, event/audit trail, secret redaction, or evidence-based completion
gate. Pin the exact ADK v2 version only after the spike passes.

## Parallel Delivery Contract

The two active workstreams must remain independently buildable, testable, and
reviewable.

### Dependency direction

- The runtime framework must never import cockpit, TUI, IDE, or Google ADK
  packages.
- Google ADK and other coding-harness dependencies must stay behind a dedicated
  cockpit adapter boundary. They may not leak into the public `ovr` API or the
  runtime package graph.
- The cockpit consumes the versioned public Ouvrier API, embedded documentation,
  scaffold contracts, and narrow internal service interfaces. It does not gain
  authority to reshape runtime behavior implicitly.
- `internal/cli` is a shared integration edge. Changes there should remain
  small and must preserve both headless runtime commands and cockpit entry
  points.
- Changes to genuinely shared provider, policy, schema, state, or tool code need
  tests for both runtime execution and cockpit construction flows.

If putting ADK in the root Go module creates unacceptable dependency, upgrade,
or supply-chain coupling for framework consumers, isolate the cockpit behind a
nested Go module or another explicit Go package/build boundary. Full Go does
not require coupling the two harnesses.

### Independent quality gates

Runtime track 0A owns:

- public API and execution-contract tests;
- provider, tool, policy, sandbox, state, schema, and trigger conformance;
- race, cancellation, recovery, retry, idempotency, and failure-injection
  suites;
- runtime benchmarks and resource-bound checks.

Cockpit track 0B owns:

- hermetic agent-loop tests with scripted or fake models;
- lossless event and tool-history tests;
- permission-gate and workspace-jail tests;
- session resume, interruption, compaction, and bounded-loop tests;
- golden construction scenarios that compile and audit generated workers
  against the current local Ouvrier framework;
- Bubble Tea model tests for the TUI and IDE clients.

The shared integration gate is deliberately narrow:

```text
cockpit creates or repairs worker
-> generated worker compiles against current ovr
-> runtime contract suite executes the worker
-> deterministic audit passes
```

A failure in that lane identifies an explicit product-contract mismatch. It
must not be fixed by silently weakening either track's acceptance criteria.

### Work that can start immediately in track 0B

1. Build the isolated Google ADK Go v2 spike described above.
2. Define the Ouvrier-specific agent, event, tool, session, and evidence
   interfaces independently of the TUI.
3. Build the embedded Ouvrier API index, prompt pack, and scaffold knowledge.
4. Prove a read-only journey: inspect and audit an existing worker.
5. Prove a write journey in a temporary workspace:
   `plan -> scaffold/edit -> compile/test/audit -> bounded repair`.
6. Connect the proven kernel to the existing TUI and IDE as clients.

These slices do not wait for the whole runtime stabilization milestone. They
target the current public runtime contract and adapt explicitly when track 0A
makes a deliberate, documented contract decision.

## Cockpit and IDE Scope While Deployment Is Paused

The active cockpit completion path is temporarily:

```text
prompt
-> inspect Ouvrier context
-> plan
-> scaffold or edit
-> compile and test
-> audit and review
-> bounded repair loop
-> verified local artifact
```

Deploy and fleet operations are not part of the active cockpit acceptance
criteria. Existing transfer/deploy integrations may remain for compatibility,
but they must not drive the cockpit rearchitecture.

The IDE remains the precise terminal editing surface nested in, or sharing the
same kernel with, the cockpit. It is a secondary slice inside track 0B: it may
progress after the core cockpit kernel is proven, without waiting for all of
track 0A and without becoming a separate agent engine or a general-purpose IDE.

## Paused: Deployment PaaS and Web Console

The agentless PaaS and web console are **fully paused** until an explicit
product redesign resumes them.

During the pause:

- do not add deployment-platform or fleet-console features;
- do not expand their public API or use them to justify runtime changes;
- do not treat the v0.3 Deploy & Scale backlog as an active milestone;
- retain shipped code for compatibility, critical security fixes, and
  maintenance only;
- keep deployment and console tests green when nearby code changes;
- document any unavoidable compatibility break instead of silently redesigning
  the platform.

The later redesign must reconsider the platform and console together, including
their product perimeter, operational model, security boundary, and relationship
to the rebuilt cockpit. Existing v0.3 design choices are inputs to that review,
not automatically binding requirements.
