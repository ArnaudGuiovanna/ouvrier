# Ouvrier as an Agentic Harness — Final Design Specification (v1.1)

**Status:** Proposed for approval. Reconciles the v1.0 synthesis with the adversarial review. Ground-truthed against `codex-cli 0.139.0`, the real `~/.codex/auth.json` schema, and cited source. All file:line citations use real package directories (`internal/tui/`, `internal/operate/`, `internal/provider/`, `internal/deploy/`). Where the review found a load-bearing claim wrong — most importantly the Codex transport — this spec adopts the corrected mechanism and re-scopes the affected slices.

> Produced by a multi-agent design panel (PM, product designer, UX, dev-experience, staff engineer) grounded in Pi (earendil-works/pi) + Claude Code and the existing Ouvrier codebase. Supersedes the UX/engine intent of the v0.5 and v0.5.1 specs.

> **Direction update — 2026-07-26:** The active direction is now defined by
> [`docs/project-direction.md`](../../project-direction.md). The cockpit must
> adopt Pi's harness architecture, not merely its interaction style, while
> remaining a fully Go coding agent specialized in constructing Ouvrier
> workers. Google ADK Go v2 is the preferred candidate for a bounded
> architecture spike around runners, sessions, events, tools, and
> sequential/parallel/loop workflows. Deployment PaaS and web-console work are
> paused. Cockpit-harness work is an active track in parallel with runtime
> stabilization; it does not wait for the complete runtime milestone and must
> stay behind a one-way dependency boundary from cockpit to runtime. Therefore
> the active cockpit journey currently ends at a compiled, tested, audited,
> reviewable local artifact; create-and-deploy journeys and deploy acceptance
> criteria below are retained as historical design material, not active
> milestone requirements.

## Implementation status (2026-06-14)

**MVP (Slices 0–4) implemented, reviewed, and merged to `main`.** Each slice was built subagent-driven (TDD, per-task spec + code-quality review) under its own plan in `docs/superpowers/plans/`:
- **Slice 0** — multi-turn history correctness (`tool_call_id` persistence + tool-turn replay). [part 1 plan]
- **Slice 1** — single entry point: bare `ouvrier` opens the cockpit; `-p` one-shot; `-c` resume-latest; agent-centric root help. [part 1 plan]
- **Slice 2** — approval/policy gate: `Governance` levels, synchronous `gate` with headless fail-closed, prod typed double-confirm, Shift+Tab posture, TUI approval card. [part 2 plan]
- **Slice 3** — Codex subscription auth (`internal/auth` probe + tolerant device-auth bridge), `internal/provider/codex` text transport over `codex exec`, auth-first `resolveAgentModel`, cockpit auth surface. Codex transport is text-only (documented §5 limitation); structured tool-calls use API-key providers. [part 3 plan]
- **Slice 4** — review IDE: `StreamReview`/`StreamDiff` events, findings inbox + read-only diff overlay (Ctrl+R), `f`→agent fix→re-audit. [part 4 plan]

**Deferred (post-MVP, not yet planned):** Slice 4.5 (full-screen human editor + gated `write/edit_worker_file`), parts of Slice 5 (`/tree /fork /clone`, deeper skill-pack), Slice 6 (inline non-alt-screen differential rendering), and the deploy-engine host-key-honesty/post-transfer-integrity *surfacing* in the approval card (the engine already pins hosts via `internal/deploy/knownhosts.go`).

---

## 1. Product definition & hero journeys

**One-liner.** Ouvrier is a terminal agent that builds, reviews, and ships production Go workers — you describe the worker in natural language, it scaffolds the code, audits it, lets you review and (later) take over, then SSH-deploys it, pausing for your approval before anything irreversible.

**Positioning.** Claude Code / Pi, but the loop ends in a *running binary on a host* with a quality gate and an approval gradient in between. The chat is not the product; the *deployed worker* is. "The product IS the agent" means: typing `ouvrier` opens the TUI; the worker tools (`internal/operate/tools.go`) become the agent's hands; the old subcommands survive only as `!ouvrier <cmd>` escape hatches.

**Scope discipline (the moat).** Ouvrier is opinionated about Ouvrier workers — not a general coding agent for arbitrary repos. `read_worker_file` staying sandboxed to the worker dir (`internal/operate/tools.go`) is correct and stays. **This thesis is binding on tool design:** any general-capability tool (`bash`, `grep`, `find`) is path-jailed to the worker/candidate dir (§5, E3 resolution). We accept that this makes `bash` deliberately limited — that is the point, not a bug.

### Hero journey 1 — Create + Deploy (headline)
Prompt: *"crée-moi un worker de veille réglementaire sur cette ICPE puis déploie sur test@HOST"* → agent plans (scaffold → audit → review → build → deploy), scaffolds a compiling worker with the right trigger, audits, surfaces findings, raises a **build** approval card (1 confirm), then a **deploy** approval card (1 confirm; prod = 2nd typed confirm), and reports host + release id + gate status.

### Hero journey 2 — Review & Repair (trust & repair)
Prompt: *"audit veille-icpe and fix anything blocking deploy"* → deterministic gate failures land in the findings inbox; the **agent** fixes via `fix_worker`; `fix` re-runs audit automatically and reports the before/after gate delta; the agent never claims "fixed" without a passing re-audit. (Human full-screen take-over editing is a post-MVP extension — see §6, §9.)

### Hero journey 3 — Edit + Review (IDE mode, phased)
Prompt: *"add rate-limiting to the fetch tool and mark it RequiresApproval"* on an existing worker → agent edits, every edit renders as a reviewable **diff viewer** (read-only in MVP), findings inbox lists issues by severity/file:line with open/fix/accept/dismiss. Human take-over via a full-screen editor (`Ctrl+E`) and the gated `write/edit_worker_file` tools land in the IDE-takeover slice (4.5), not MVP.

---

## 2. First-run & Codex subscription auth (the priority)

**Decision: lazy, never-blocking, Codex-bridge-first.** The TUI opens immediately; auth is a dismissible card, not a gate. We **bridge the official Codex CLI** and reuse `~/.codex/auth.json` — we never write or scrape tokens, and we never call OpenAI's private backend ourselves (see §5, A1). Codex owns credentials, refresh, billing, and ToS.

**Honest framing (review A1).** Login copy must **not** claim "endorsed by OpenAI / Codex-for-OSS." We route model turns through the official `codex` client, which is the legitimate subscription path. The card says only what is true: *"Routed through the official Codex CLI, billed to your ChatGPT subscription. Credentials stay owned by Codex."*

**Boot probe → 4-state machine** (computed at launch by an extended probe):

```
launch → AuthProbe() ─┬─ AUTHED      → empty composer, footer "model · signed in as <acct>"
                      ├─ UNAUTHED    → onboarding card (composer still focusable)
                      ├─ NO_CODEX    → install card (npm i -g @openai/codex)
                      └─ NO_ENTITLE  → quota/entitlement card (plan lacks Codex / limit hit)
```

**Probe reads minimum, prefers the official status command (review A3).** `internal/auth` reads only `auth_mode` + `tokens.account_id` from `auth.json` for a fast presence/label probe — **never** `access_token`/`refresh_token`. Source of truth for "actually usable" is `codex login status` (verified: prints `Logged in using ChatGPT`). Extend `Probe` in `internal/operate/codex/driver.go` (today hardcodes `Authenticated: false`, "proven only during a real turn") to run `codex login status` (cheap, no model call) and parse the account label. We never load OAuth tokens into our process because, under the bridge transport (§5), Codex makes the calls — we have no use for them.

**Verified `auth.json` schema (the contract).** Top-level keys: `auth_mode`, `OPENAI_API_KEY`, `tokens`, `last_refresh`. `tokens` = `{id_token, access_token, refresh_token, account_id}`. (The v1.0 doc's nesting of `OPENAI_API_KEY` under `tokens` was wrong; corrected here.)

**Device-auth orchestration — new `internal/auth/codex.go`** (reuses the `codex.Runner` seam in `internal/operate/codex/driver.go`):
```go
type Login struct{ Runner codex.Runner }
func (l *Login) Start(ctx context.Context) (<-chan DeviceEvent, error)
type DeviceEvent struct { Code, URL, Raw string; Status LoginStatus } // Pending|Success|Failed|Timeout
```
**`codex login --device-auth` is a flag, not a subcommand (verified, review A2).** In 0.139.0, `codex login` exposes only `status`/`help` subcommands; `--device-auth`, `--with-api-key`, `--with-access-token` are flags on `codex login`. **Hard prerequisite before Slice 3 estimation:** capture the real `codex login --device-auth` stdout/stderr on a fresh machine and pin the parser with a fixture test. The `DeviceEvent.Raw` field always carries the unparsed stdout so the card can degrade gracefully (show raw output) if the format shifts. If the format proves unstable, fall back to shelling `codex login` and letting Codex own the UX rather than scraping.

We shell `codex login --device-auth`, parse the device code + `https://auth.openai.com/codex/device` URL with a tolerant scanner, render as **large copyable text, never QR-only** (headless/SSH is first-class), and poll `codex login status` on a `tea.Tick` every 2s until success or the 15-min TTL. On success: dismiss card, toast `✓ Signed in as <account> — OpenAI models ready`.

**Six friendly cards (exact copy intent):**
1. **NO_CODEX** — install + `[r]` retry / `[k]` api key.
2. **UNAUTHED** — `[enter]` sign in / `[k]` api key.
3. **DEVICE CODE** — URL + code + waiting spinner + `[esc]` cancel.
4. **TOKEN EXPIRED** — `[enter]` re-sign-in / `[k]` api key.
5. **NO NETWORK** — `[r]` retry.
6. **QUOTA / ENTITLEMENT (review A4)** — *"Your ChatGPT plan doesn't include Codex usage, or you've hit a usage limit. Press `[k]` to use an API key."* Mapped from the Codex "usage limit"/entitlement error.

The **#1 real-world device-auth failure** — device-auth disabled in the ChatGPT workspace — gets specific remediation on card 3's failure path: *"Device sign-in may be disabled for your ChatGPT workspace. Ask an admin to enable it, or press [k] to use an API key."*

**Token-expired is also a turn-time event.** If a turn fails auth mid-session, map it (seam: `mapCodexErr` in `internal/operate/codex/driver.go`) to `StreamError{Kind: AuthExpired}`, re-raise the EXPIRED card **without losing the queued turn**, re-run after re-auth. Because the bridge transport (§5) routes through `codex exec`, expiry detection is defined against that transport's exit/stderr contract, not a raw HTTP 401 (review E3).

**Repurpose `login_codex`** (`internal/operate/tools_worker.go`): keep the metadata write (feeds `/session`) but make it actually drive `internal/auth` and back the TUI card.

**Honest entitlement caveat.** AC-A3 ("no API key ever required") holds **only for plans that include Codex**. For plans without it, the QUOTA/ENTITLEMENT card routes the user to the API-key fallback. We state this plainly rather than over-promise.

---

## 3. TUI architecture & visual language

**Core thesis: one continuous transcript with overlays.** Kill the `mode string` machine (`internal/tui/operate.go:79`, "select | operate | factory") in favor of an `overlay` enum (`none | auth | review | editor`) composed over the base transcript. The cockpit (`operateModel`) is the home surface; auth, review-IDE, and editor are overlays that don't destroy the transcript.

**Rendering decision.** Base transcript renders **inline (non-alt-screen)** with `CSI ?2026h/l` synchronized output and per-line SGR/OSC-8 resets — this preserves native scrollback/copy/search, the #1 "feels SOTA" detail. Alt-screen is reserved **only** for overlays. **MVP ships the three journeys in alt-screen**; inline rendering is explicitly deferred to the polish slice and must never block a journey (review E2 confirms the cut). Set `view.AltScreen` (`internal/tui/operate.go:753`) `true` unconditionally until the polish slice lands, then `true` only when an overlay is active.

**Palette — extend `internal/tui/colors.go` with exactly 4 tokens:**
```
magentaHex = "#c678dd"  // approval / human-in-the-loop gate (reserved: "act now")
diffAddBg  = "#0e2a1c"  // + line bg     diffDelBg = "#2a1216"  // - line bg
gutterHex  = "#3a4250"  // diff/editor gutter
```
**Semantic color contract:** green=success/safe/additions/ReadOnly · cyan=the agent · yellow=caution/Idempotent/SideEffecting · red=failure/blocking/deletions · **magenta=a human must decide now (approval gates only)** · muted=chrome.

**Glyphs:** `❯` user · `●` agent · `⚙` tool · `✓` ok · `✗` fail · `▌` caret · `◆` approval gate · `▸/▾` collapse · `◷` running. Severity: `■` critical(red) · `▲` high(red) · `●` medium(yellow) · `○` low(muted) · `⊘` dismissed.

**Four regions (keep the frame from `internal/tui/operate_view.go`):** transcript / slash-menu / composer / **status bar = governance HUD**. The status bar shows model · git branch · session · queued · **posture segment** (magenta when `manual ◆`, green when `auto-safe`, muted in `plan`) · working verb+spinner + `esc to interrupt`. Tool cards (`renderBlockTool`, `opBlock`) get `collapsed bool` (`Ctrl+O` toggle) and a governance-tinted left bar (green/yellow/magenta).

### Mockup A — First-run login (overlay over empty transcript)
```
┌─ ouvrier ─────────────────────────────────────────────────────────────────────┐
│ ◢ Ouvrier Agent Cockpit — prompt → plan → build → review → deploy.              │
│   ╭──────────────────────────────────────────────────────────────────────╮     │
│   │  ◆  Sign in to use OpenAI models — zero API key                        │     │
│   │  Routed through the official Codex CLI, billed to your ChatGPT          │     │
│   │  subscription. Credentials stay owned by Codex.                        │     │
│   │  1. Open this link and sign in:                                        │     │
│   │     https://auth.openai.com/codex/device                               │     │
│   │  2. Enter this one-time code   (expires in 15 min)                     │     │
│   │     ┌───────────────────┐                                              │     │
│   │     │   A B C D - E F G  │   ← selectable, ctrl+y to copy              │     │
│   │     └───────────────────┘                                              │     │
│   │     ◷ Waiting for browser sign-in…  (polling codex login status)       │     │
│   │  enter open browser   ·   k use API key   ·   esc skip                 │     │
│   ╰──────────────────────────────────────────────────────────────────────╯     │
│   Tip: device sign-in must be enabled in your ChatGPT/workspace security.       │
├──────────────────────────────────────────────────────────────────────────────┤
│ ❯ Describe a worker to build…                                                    │
├──────────────────────────────────────────────────────────────────────────────┤
│ ◷ signing in…  ·  model —  ·  worker .  ·  posture manual ◆                      │
│ enter open browser   k api key   esc skip   ctrl+c quit                          │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Mockup B — Create + deploy turn (approval gate with honest host-key state)
```
❯ crée-moi un worker de veille réglementaire sur cette ICPE puis déploie sur test@HOST

● I'll scaffold an HTTP+cron worker that polls AIDA/Géorisques for this ICPE, diffs
  against last-seen state, and replies with a digest.
  Plan: scaffold → audit → you review → build → deploy to test@HOST.

 ⚙ scaffold_worker                                              ✓        │ green
 │ name: veille-icpe   trigger: cron @daily + http GET /digest  → 6 files
 ⚙ audit_worker                                                 ✓     ▾  │ green
 │ → 1 medium, 0 blocking · governance OK · all tools classified
 │ ● medium  feeds.go:42  external HTTP call not marked Idempotent
 ⚙ build_worker                                                 ◷ …    │ yellow
 │ target: linux/amd64    → compiling veille-icpe…

   ╭─ ◆ Approval required ───────────────────────────────────────────────╮
   │ DEPLOY  veille-icpe  →  test@HOST                                     │
   │   binary    veille-icpe (linux/amd64, 14.2 MB, sha256 9af2…)          │
   │   transport ssh test@HOST:22                                          │
   │   host key  ⚠ FIRST CONTACT — verify fingerprint out-of-band:         │ magenta
   │             SHA256:nTh…q0  (will pin on approve)                      │
   │   release   2026-06-14T0931Z-a1b2c3 · keep last 5                     │
   │   units     systemd: veille-icpe.service (enable + restart)          │
   │   integrity remote sha256 re-checked after transfer, before restart   │
   │   audit     1 medium, 0 blocking — passes gate                       │
   │   ◆ enter approve     e edit args     d dry-run/diff     esc deny     │
   ╰──────────────────────────────────────────────────────────────────────╯
──────────────────────────────────────────────────────────────────────────────
❯ ▌
⚙ working 0:18 · model codex/gpt-5-codex · worker veille-icpe · git main* · posture manual ◆
esc interrupt   alt+enter queue follow-up   ◆ approve gate   d diff   ? help
```
On a **previously-pinned** host the line reads `host key  pinned ✓  SHA256:nTh…q0 (seen before)` in green. Prod target adds a second card requiring a **typed** confirmation: `Type "icpe-prod-01" to confirm prod deploy: ____`, and prod **first contact** requires explicit fingerprint confirmation, never auto-pin (review C2).

### Mockup C — Review IDE (MVP: findings inbox + read-only diff viewer)
```
┌─ Review — veille-icpe ──────────────────────────────────  3 findings · 1 blocking ─┐
│ FINDINGS                          │ feeds.go                              ▸ diff   │
│ ◉ ■ critical feeds.go:51          │ @@ -38,9 +38,14 @@ func fetchAIDA(...) {        │
│   No timeout on outbound HTTP     │  38   client := &http.Client{}                 │
│   ▲ high     pipeline.go:18       │ -40   resp, err := client.Get(feedURL)         │ red bg
│   Tool not classified             │ +40   ctx, cancel := context.WithTimeout(      │ grn bg
│   ● medium   feeds.go:42          │ +41       r.Context(), 10*time.Second)         │ grn bg
│   HTTP call not Idempotent        │ +42   defer cancel()                           │ grn bg
│   ⊘ low      README.md:1          │ ───────────────────────────────────────────── │
│                                   │ FINDING ■ critical · feeds.go:51               │
│                                   │ FIX  wrap client.Get in context.WithTimeout.   │
├───────────────────────────────────┴────────────────────────────────────────────┤
│ ↑↓ findings  o open  f fix(agent)  a accept  x dismiss  d cycle layout            │
│ tab pane    enter jump to file:line    q back to chat                               │
└──────────────────────────────────────────────────────────────────────────────────┘
```
The full-screen **editor** pane (`E` → `ctrl+s save & re-audit`, `ctrl+g $EDITOR`) and the gated write tools it requires are **post-MVP (Slice 4.5)**. MVP fixes flow through the agent's `fix_worker`.

---

## 4. Interaction model

**Replace the ad-hoc `switch keyStr` (`internal/tui/operate.go:430`) with a context-scoped `key.Binding` table** (the `key` package is already imported). Surfaces: `composer / streaming / approval / review-inbox / diff / editor / overlay`. Promote `running bool` (`internal/tui/operate.go:87`) to `turnState enum{idle, streaming, awaitingApproval, executingSideEffect}` — the last state is new and load-bearing for the steer-safety rule below.

**Global keys (composer + streaming):**
| Key | Action |
|---|---|
| `Enter` | Submit; mid-stream **only during `streaming` text/ReadOnly** w/ empty queue = **steer** (interrupt+inject). Inert/queue-only during `awaitingApproval` and `executingSideEffect` (review D1). |
| `Shift+Enter` | Newline (remap from current Alt+Enter) |
| `Alt+Enter` | **Queue follow-up** while streaming (path exists `internal/tui/operate.go:519`) |
| `Alt+Up` | Restore last queued message into composer |
| `Esc` | Interrupt current turn (no-op if idle). Semantics depend on `turnState` — see steer/abort below. |
| `Ctrl+C` | 1st: clear composer; 2nd within 2s (empty): quit — never instant kill mid-deploy |
| `Ctrl+D` | Quit when composer empty (move its current scroll to `Ctrl+V`) |
| `Ctrl+L` / `Ctrl+P` | Model picker overlay / quick-cycle (both rebind the **live** runtime) |
| `Shift+Tab` | **Cycle approval posture:** manual → auto-safe → plan |
| `Ctrl+O` / `Ctrl+T` | Collapse/expand focused tool card / thinking blocks |
| `@` / `!cmd` / `!!cmd` | Fuzzy file ref / shell-into-context / shell-silent (hidden CLI escape hatch) |

Adopt Pi's emacs line-editing wholesale (`Ctrl+A/E/B/F`, `Alt+B/F`, `Ctrl+W/U/K/Y`, `Ctrl+-`).

**Slash commands — split harness vs direct-action** (today's `/build /deploy /review` at `internal/tui/operate.go:112` are verbs that should be agent tool-calls):
- **Harness/session:** `/login /logout /model /models /new /resume /name /session /compact /context /cost /approve /policy /copy /export /help /quit` (MVP). `/tree /fork /clone` are **post-MVP** (review E2).
- **Direct-action (still flow through the gate):** `/scaffold /audit /diff /review /fix /build /deploy /docs /api /workers /read /edit`

One combined completion provider handles `/`, `@`, and bare model names (generalize `updateSlash`, `internal/tui/operate.go:499`).

**Steer / queue / abort — precedence is explicit (review D1, D2, E3).**
- During `streaming` (text/ReadOnly tool phases): composer never locks. `Enter` with empty queue = steer (interrupt+inject); `Alt+Enter` = queue; `Esc` = abort the turn, emit `⊘ interrupted`, preserve partial output, return to idle. Esc cancels ctx **between** tool calls.
- During `awaitingApproval`: `Enter` is **not** steer — it routes to the focused approval card (approve). `Esc` = deny the gate (clean, no side effect has begun). A queued follow-up via `Alt+Enter` is allowed.
- During `executingSideEffect` (a `build_worker`/`transfer_worker` is mid-flight): `Enter` is inert. `Esc` maps to **rollback intent**, not raw ctx-cancel (review C4): the deploy engine's `internal/deploy/rollback.go` is invoked at the next safe checkpoint; if no bytes have landed yet it degrades to a clean cancel. The card shows `esc rollback` during this phase.
- **Two blocking interactions on one input (review E3):** when a gate is awaiting a decision, `Esc` always resolves the *gate* first (deny), never the background stream. The gate decision and the interrupt share a single precedence: gate-deny > stream-interrupt.

**Confirmations (the gating layer, §5).** ReadOnly → auto · SideEffecting/build → 1 confirm · deploy → 1 confirm · **prod → 2nd confirm with typed token**. Approval card keys: `Enter/y` approve · `Esc/n` deny (reason returned to model) · `e` edit args · `a` (auto-safe posture only) approve-for-session. **The prod second confirm requires typing the host/worker name — no accelerator can skip it.**

---

## 5. Agent engine

**Kernel: one brain, three frontends.** Keep `AgentRuntime.RunTurn` (`internal/operate/runtime_stream.go:49`) as the *only* engine entry; TUI, `ouvrier -p`, and `--mode rpc` are pure consumers of its `StreamEvent` channel. `runPrompt` (`internal/operate/runtime.go:314`) stays the single execution path. **Kill the dual brain:** demote the keyword planner behind `OUVRIER_NO_MODEL` (test/offline only); the default product path is always the model loop. One brain = one place to gate, one place to persist.

**Fix lossy history first (Slice 0).** `historyMessages` (`internal/operate/agent_loop.go:153`) replays only assistant *text* and drops tool turns — fatal for the inherently multi-turn core journey. Persist `TranscriptToolCall`/`TranscriptToolResult` with model IDs and rebuild full provider history (`provider.AssistantToolCalls` + `provider.ToolResultText`, already used live at `internal/operate/agent_loop.go:128,139`).

### Codex transport — the corrected mechanism (review A1, the big fix)

**v1.0 was wrong.** The synthesis proposed a new provider POSTing to `https://chatgpt.com/backend-api/codex` (Responses API) with re-read OAuth tokens. That is **reverse-engineering OpenAI's private backend** — version-coupled, ToS-risky, and dishonest to advertise as "endorsed." We do not do this by default.

**Default transport = the official Codex client.** Route model turns through **`codex exec`** (verified: `codex exec [OPTIONS] [PROMPT]`, reads prompt from stdin, `-m model`, JSON output modes) — or **`codex mcp-server`** (Codex-as-MCP-over-stdio, also present in 0.139.0) if the streaming/tool-call ergonomics prove cleaner. The repo already shells Codex via `internal/operate/codex/driver.go` with a clean `Runner` seam; this transport extends that seam. **Billing, token refresh, and ToS stay entirely inside the official client.** We bridge it; we never impersonate its browser.

- **New `internal/provider/codex`** registered under the `codex/` prefix so `Registry.ForModel` (`internal/provider/registry.go:40`) routes automatically. It implements `Complete` + `CompleteStream` by driving `codex exec`/`mcp-server` via the `Runner`, translating Codex's streamed JSON events into `provider` stream chunks and tool-call shapes.
- **The raw `backend-api` Responses transport is removed from the default path.** If ever revived, it ships as an explicitly-labeled, opt-in, *unsupported* experiment — never the default, never described as endorsed. This deletes the Responses codec / `tokenSource` / per-request 401-refresh work from Slice 3.
- **Keep the Codex *exec* driver's existing role** for sandboxed coding (`patch/fix/review_worker`) — note the transport and the coding-agent driver now both ride the same official client, which is exactly the consistency we want.

**Model resolver — replace `operateModelFromEnv` (`internal/tui/operate_model.go:16`) with `resolveAgentModel`:** (1) Codex client signed in (`codex login status` ok) → Codex transport [preferred, zero-key] → (2) explicit `--model provider/x` with that provider's key → (3) `OPENAI_API_KEY`/`ANTHROPIC_API_KEY` fallback → else UNAUTHED card. Auth state beats env (Pi parity).

**Model catalog — curated, not discovered (review D3).** There is no documented endpoint to enumerate the models a ChatGPT account may use, and `codex exec` doesn't enumerate accepted `-m` values either. **MVP ships a curated, version-pinned list** and treats dynamic discovery as a non-goal. **Delete the fictional `codex/gpt-5.5`** (`internal/tui/operate.go:261-264`). **Fix `cycleModel`** (`internal/tui/operate.go:596`) to rebind the live `AgentModel` via a new `AgentRuntime` model setter (today `Model` is fixed in `RuntimeOptions`, `internal/operate/runtime.go:35`).

### Governed tools + ToolGate (safety is a runtime primitive, not a tool)

1. Replace `Tool.ReadOnly bool` (`internal/operate/tools.go:21`) with a `Governance` enum mirroring the framework's own levels: `ReadOnly | Idempotent | SideEffecting | RequiresApproval`. Map: `list/read_*/diff/audit/review` = ReadOnly; `scaffold/patch/fix/write/edit` = SideEffecting; `build_worker` = SideEffecting+confirm; `transfer_worker` = RequiresApproval (prod → 2nd confirm).
2. Insert a synchronous `r.gate(ctx, session, call, emit) (decision, error)` at **both** `callTool` sites — `internal/operate/agent_loop.go:138` and `internal/operate/runtime.go:383` — so model and planner paths stay consistent. The model cannot route around it (unlike `accept_risk`, which is model-driven and stays only as a rationale recorder).
3. New `StreamApprovalRequest` event (`internal/operate/runtime_stream.go:15`) carrying the exact action (SSH host, **host-key state**, binary path+sha, env, diff summary); thread an operator-decision channel through `RunTurn`/`runPrompt` (extend the `emit` closure pattern at `internal/operate/runtime_stream.go:59` with a paired `await(decision)`). The goroutine blocks; the gate decision resolves **before any side effect begins** (review C4).
4. **Posture only narrows what auto-passes.** RequiresApproval always prompts; prod always double-prompts — regardless of posture. Non-negotiable safety floor.
5. **Headless / non-interactive contract (review C1 — new, explicit).** When there is **no operator decision channel** attached (`ouvrier -p`, `--mode rpc`, CI): RequiresApproval and prod targets **fail closed**. They proceed only when an explicit, separately-audited token is supplied (`--yes-i-understand=<token>` or a policy file naming the exact host/worker). `--allow-failed` (`internal/operate/tools_worker.go:158`) remains a CI escape hatch for *audit* failures only — it never bypasses an approval gate. Default-allow is forbidden.

**Deploy integrity & host-key honesty (review C2, C3) — engine-level requirements:**
- The approval card distinguishes **`pinned ✓` (seen before, fingerprint shown)** from **`⚠ FIRST CONTACT` (TOFU — verify out-of-band, will pin on approve)**. A green "pinned ✓" must never appear on first contact. **Prod first contact requires explicit fingerprint confirmation; it never auto-pins.** Backed by `internal/deploy/knownhosts.go` + `RequirePinnedHost` (`internal/deploy/ssh.go`).
- **Post-transfer integrity check:** after `transfer_worker` lands bytes and **before** systemd enable/restart, the remote sha256 is recomputed and compared to the local artifact. Mismatch = hard abort + rollback. Surface the verified remote sha in the success report.

**Add Pi-parity gated tools — with the moat held (review E3):** `read/write_worker_file/edit_worker_file/bash/grep/find/ls`, all **path-jailed to the worker/candidate dir** reusing the guard at `internal/operate/tools.go:183`. We explicitly accept that `bash` is therefore limited (no arbitrary host access) — that *is* the scope-discipline thesis, stated rather than hand-waved. Fill the 6 missing `toolSchemas` (`internal/operate/agent_loop.go:215`). The write tools (`write/edit_worker_file`) are **post-MVP (Slice 4.5)**, gated SideEffecting.

**Native Ouvrier skills / prompt-pack.** Keep `ouvrierSystemPrompt` (`internal/operate/promptpack.go:12`) thin; add an embedded `internal/operate/skills/` (`embed.FS`). **MVP ships ONE skill** — `ouvrier-core.md` (From/Pipe/Tool/Reply/Output + a worked example) — because AC-D1 (first-try-correct scaffold) depends on it. The other four (`governance.md`, `triggers.md`, `deploy-env.md`, `scaffold-recipes.md`) land incrementally and must not block MVP (review E2). The core prompt names the skills and instructs the model to `read_ouvrier_api(skill)` **before** scaffolding/editing. Rewire `read_ouvrier_api` (`internal/operate/tools.go:162`) and `search_ouvrier_docs` (`internal/operate/tools.go:136`) to read/index this pack. Skills are version-pinned in the repo and are the single source of truth — **no fictional symbols.**

**The framework is embedded in the binary (load-bearing — review/owner emphasis).** The harness *is* an Ouvrier-framework expert that ships the framework with it. The `ouvrier` binary embeds, via `embed.FS`, three artifacts **version-locked to the exact framework version compiled into that binary** so the agent's knowledge and the code it generates can never drift from the API it ships with:
1. **The skill pack** (`internal/operate/skills/*.md`) — the authoritative Ouvrier API reference the model reads before scaffolding/editing (`read_ouvrier_api`/`search_ouvrier_docs` index this, not the on-disk `docs/`).
2. **The scaffold templates** — `scaffold_worker` emits idiomatic Ouvrier Go from these embedded templates (no hidden DSL), so a "create a worker for X" prompt always produces a worker that compiles against the framework version in the same binary.
3. **An API symbol index** — the exact public symbols (`From/Pipe/Tool/Reply/Output`, governance markers, trigger constructors) so the agent cites real symbols and never invents API.

This is the moat: the agent doesn't *guess* Ouvrier — it carries the framework's API and recipes inside itself, version-pinned, and turns a natural-language brief into a bespoke, compiling Ouvrier worker. A release of the binary and the framework knowledge it embeds move together.

**Verification floor:** the agent **never claims "fixed" without a passing re-audit** — close the loop with a verification re-run (§6).

---

## 6. Review IDE data flow

The data exists (`Finding{Severity,File,Line,Title,Body,Action}` `internal/operate/review.go:28`, `ReviewReport`, `ReviewHasBlockingFindings` `internal/operate/review.go:176`, `ObserveCandidateDiff` `internal/operate/tools_worker.go:140`); the UI and write-back loop are missing.

**Two new event kinds:** `StreamReview` (carries `[]Finding`) and `StreamDiff` (carries unified-diff hunks), emitted by `review_worker`/`diff_worker` so the TUI renders **panes, not flat `renderBlockTool` cards**.

**MVP sub-models (overlay = `review`):**
- **Findings inbox** — `SelectList` keyed severity→file:line. Actions map to tools: `open`→jump diff viewer to file:line · `fix`→`fix_worker(subject=finding)` · `accept`→record resolution · `dismiss`→mark state, dimmed `⊘` (auditable, never silently dropped). Add finding-state persistence to `ReviewReport`.
- **Diff viewer (read-only in MVP)** — reuses `internal/operate/patch.go`; `+`/`-` gutters with `diffAddBg`/`diffDelBg`, `@@` hunk headers, per-file collapse.

**Post-MVP (Slice 4.5):**
- **Editor** — full-screen takeover over the candidate dir, requiring the new gated `write_worker_file`/`edit_worker_file` (SideEffecting). `ctrl+g` hands off to `$EDITOR`.

**Finding-state → deploy-block policy (review D4 — the actual safety boundary, now explicit).** `ReviewHasBlockingFindings` is deterministic; the human-override on top of it is the boundary:
- `dismiss` dims a finding for triage but **does not** clear a deploy block and **does not** record accepted risk.
- `accept` clears the deploy block **for non-prod targets only**, and records accepted risk (auditable via `accept_risk`).
- **On a prod target, critical/high blocking findings cannot be cleared by `accept` — only by `fix` + a passing re-audit.** This is tested (AC-R5).

**The closing loop (what makes it an IDE):** `fix` (MVP) or editor write (4.5) → auto re-run `audit_worker` + `review_worker` → emit fresh `StreamReview`/`StreamDiff` → inbox refreshes. Write/fix → re-audit → re-review is a cycle, and a "fixed" claim requires the re-audit to pass.

---

## 7. Build & SSH deploy with per-step confirmations

**Reuse `internal/deploy/*` wholesale** — no new deploy code, with two *additions* required by the safety review (host-key honesty + post-transfer integrity, §5). Already wired via `Harness.Build` (`internal/operate/harness.go:212`) and `Harness.Transfer` (`internal/operate/harness.go:232`) → `build_worker`/`transfer_worker` (`internal/operate/tools_worker.go:147,169`). Pinned-host SSH (`internal/deploy/ssh.go` `RequirePinnedHost`, `internal/deploy/knownhosts.go`), release IDs/layout (`internal/deploy/release.go`), rollback (`internal/deploy/rollback.go`), systemd units, preflight, env handling all stay.

**Flow with gates:**
1. `build_worker` (SideEffecting) → audit must pass first; high/critical findings block the build gate until accepted (non-prod) or fixed. Approval card shows target, audit status. **1 confirm.**
2. `transfer_worker` (RequiresApproval) → approval card shows binary path+sha256, SSH host with **honest host-key state** (pinned vs first-contact), release id, systemd units, audit+review status, and the post-transfer integrity-check promise. **1 confirm.**
3. **Prod env** (matched by deploy rule) → after the first card, a **second** red card requiring the typed host/worker name. Prod **first contact** additionally requires explicit fingerprint confirmation. **No accelerator skips either.**
4. **Execution boundary:** the gate resolves before any byte moves. Once execution starts, `Esc` = rollback intent (§4), not raw cancel. Post-transfer remote-sha mismatch = hard abort + rollback.
5. Deny returns the reason to the model; the turn aborts cleanly. On success, the agent reports host + release id + verified remote sha + gate status in one paragraph.

`d` on the deploy card runs a dry-run; `e` edits the tool args before execution.

---

## 8. What is removed / kept from today's CLI

**Single entry point:** `app.run` empty-args (`internal/cli/app.go:92`) launches the cockpit. Add `ouvrier -c` (resume) and `ouvrier -p "…"` (one-shot, reuse `runOperatePromptMode` `internal/tui/operate.go:95`). Non-interactive paths honor the **headless fail-closed gate contract** (§5).

**Removed from the product surface (kept as hidden escape hatch):** the root-help listing and the *requirement* of a subcommand. `new/build/deploy/show/status/logs/trace/add/server/fleet/console/state` stay in the `switch` (`internal/cli/app.go:97`) for CI/debug, removed from `printRootHelp`, reachable via `!ouvrier <cmd>` in the composer.

**Kept (reuse as-is):** streaming kernel (`internal/operate/runtime_stream.go`), agent loop (`internal/operate/agent_loop.go`), tool registry (`internal/operate/tools.go`), provider layer (`internal/provider/provider.go`,`registry.go`), deploy engine (`internal/deploy/*`), Codex client bridge for both transport and sandboxed edits (`internal/operate/codex/driver.go`), review/audit data (`internal/operate/review.go`,`audit.go`), Bubble Tea cockpit scaffold (`internal/tui/operate.go`).

**Refactored:** `Tool` (add `Governance`), `login_codex` (drive real auth), `cycleModel` (live rebind), `promptpack.go` (→ skills pack), `operateModelFromEnv` (→ `resolveAgentModel`), `historyMessages` (replay tool turns), `Probe` (real auth status via `codex login status`).

---

## 9. Migration plan — vertical slices from v0.5.1

Order front-loads correctness, then de-risks the safety gate **against an existing provider before betting on the Codex transport** (review E1). Each slice ships an end-to-end usable increment.

- **Slice 0 — History correctness.** Fix `historyMessages` (`internal/operate/agent_loop.go:153`). *Done when: scaffold then "now audit it" works without re-stating context.*
- **Slice 1 — Single entry point.** `internal/cli/app.go:92` → cockpit; `ouvrier -c`/`-p`; subcommands → hidden escape hatch. *Done when: typing `ouvrier` opens the TUI.*
- **Slice 2 — Confirmation/policy layer (against an existing provider).** `Tool.Governance`, `ToolGate`, `StreamApprovalRequest` + decision channel, approval cards, prod double-confirm, host-key honesty, post-transfer integrity, headless fail-closed contract, Shift+Tab posture. Runs on an existing Anthropic/OpenAI key so the safety story is decoupled from the Codex dependency. *Done when: build prompts once, prod deploy prompts twice, deny aborts cleanly, headless RequiresApproval fails closed without a token.*
- **Slice 3 — Codex transport + auth-first.** New `internal/provider/codex` over `codex exec`/`mcp-server`, `internal/auth` (presence probe + device-auth), the six cards, `resolveAgentModel`, real `Probe`. **Gated on the Slice-3-prereq fixture:** the captured real `codex login --device-auth` stdout. *Done when: a key-less user on a Codex-entitled plan logs in via device code and runs an OpenAI model; entitlement-less plans get the quota card and the API-key path.*
- **Slice 4 — Review mode (read-only).** `StreamReview`/`StreamDiff`, findings inbox, read-only diff viewer, agent `fix_worker` write-back, auto re-audit/re-review loop, finding-state→deploy-block policy (incl. prod no-accept rule). *Done when: a finding can be opened, fixed by the agent, and the inbox refreshes with the gate delta.*
- **Slice 4.5 — IDE take-over (post-MVP).** Gated `write/edit_worker_file`, full-screen editor pane, `$EDITOR` handoff, save→re-audit. *Done when: a human can edit a file in-app, save, and see findings refresh.*
- **Slice 5 — Session UX + skill-pack depth.** `/resume`/`/new` are MVP; deepen `promptpack.go` with the remaining four skills; fill the 6 missing `toolSchemas`; live model rebind hardening. `/tree /fork /clone` (over a new `parentId`; runtime Fork/Resume/Compact/Interrupt exist at `internal/operate/runtime.go:230-290`) are **post-MVP**. *Done when: `/model` changes the next turn's model and `/resume` restores full context.*
- **Slice 6 — Render polish (post-MVP).** Inline non-alt-screen differential rendering (`CSI ?2026`), collapsible thinking/tool blocks, `@` file refs, `!cmd`, queued follow-ups. *Done when: scrollback/copy/search work natively and the live region repaints without flicker.*

**MVP = Slices 0–4.** Everything labeled post-MVP (4.5, parts of 5, all of 6) is explicitly out of the first credible slice.

---

## 10. Risks & non-goals

**Risks & mitigations.**
- *Codex transport legitimacy/drift* → **resolved by design:** default transport is the official `codex exec`/`mcp-server` client, not a private backend; billing/refresh/ToS stay inside Codex. The raw backend transport is cut from the default path.
- *`codex login --device-auth` stdout format unconfirmed* → **hard prereq fixture** before Slice 3; `DeviceEvent.Raw` graceful-degrades to raw output; fallback is to let Codex own the login UX.
- *Entitlement/quota* → QUOTA card + API-key fallback; AC-A3 scoped honestly to Codex-entitled plans.
- *Token-expiry detection against the bridge transport* → defined against `codex exec` exit/stderr, not a raw HTTP 401; EXPIRED card re-raises without losing the queued turn.
- *Device-auth disabled per workspace* (the #1 device failure) → specific remediation + `[k]`.
- *Lossy history regressions amplify everything* → Slice 0 ships first with a tool-turn-replay test.
- *Model routing around the gate* → gate is a synchronous runtime primitive at both `callTool` sites; posture can't weaken the RequiresApproval/prod floor; headless fails closed.
- *Host-key TOFU false assurance* → card distinguishes first-contact from pinned; prod first-contact requires fingerprint confirmation.
- *Mid-deploy interrupt corruption* → gate resolves before side effects; in-flight `Esc` = rollback at a safe checkpoint, not raw cancel; post-transfer sha verification.
- *Steer footgun* → `Enter`-steer disabled during approval and side-effecting execution.
- *Moat erosion via `bash`* → all general tools path-jailed to the worker dir; the limitation is intentional and stated.
- *Skill-pack drift* → skills version-pinned, single source of truth, no fictional symbols.

**Non-goals (do not build):** general-purpose coding agent for arbitrary repos; web/desktop UI (terminal only); a model marketplace (Codex first, keys as fallback, existing providers unadvertised); **dynamic model-catalog discovery** (curated pinned list instead); a default raw-`backend-api` Codex transport (cut); replacing the deploy engine; a custom worker DSL (generated code stays idiomatic Go); multi-host/fleet TUI orchestration (later); session tree/fork/clone, full-screen human editor, inline rendering, branch summarization, inline images / `/share` gist (all later).

---

## 11. Acceptance criteria

**Auth (§2):** **AC-A1** First launch with no usable Codex session shows the device-auth card (large copyable code + URL, parsed from the *captured-and-fixtured* `codex login --device-auth` output), polls `codex login status`, toasts the account on success. **AC-A2** The card is dismissible (`esc`); precedence Codex-signed-in > env. **AC-A3** No API key is required to reach a deployed worker **on a Codex-entitled plan**; entitlement-less plans get the QUOTA card + API-key path. **AC-A4** Device-auth-disabled shows the specific remediation, not a generic error. **AC-A5** Mid-session expiry re-raises the EXPIRED card without losing the queued turn; expiry is detected via the `codex exec` transport contract. **AC-A6** The probe reads only `auth_mode`+`account_id`; it never loads OAuth tokens. **AC-A7** No login/auth copy claims OpenAI endorsement.

**Entry & engine (§5,§8):** **AC-E1** Typing `ouvrier` opens the TUI; `-c` resumes, `-p` runs one-shot. **AC-E2** Old commands callable only via `!ouvrier <cmd>`, absent from root help. **AC-E3** Scaffold then "now audit it" retains full tool context. **AC-E4** `/model` / Ctrl+L change the *next turn's* model; `codex/gpt-5.5` no longer appears. **AC-E5** The default Codex transport drives `codex exec`/`mcp-server`; no call is made to a private OpenAI backend.

**Framework embedding (§5):** **AC-X1** The skill pack, scaffold templates, and API symbol index are embedded via `embed.FS` and version-locked to the framework compiled into the same binary (offline-capable, no on-disk `docs/` dependency). **AC-X2** A generated worker compiles against the framework version in the same binary, and the agent cites only real public symbols (no invented API).

**Create+deploy (§7):** **AC-D1** One French/English NL prompt produces a compiling worker with the correct trigger (depends on `ouvrier-core.md`). **AC-D2** build and deploy each pause on an approval card showing the exact action; nothing runs without a keypress. **AC-D3** Audit runs before the build gate; high/critical block it until accepted (non-prod) or fixed. **AC-D4** Prod deploy triggers a second typed confirm naming the env; no accelerator skips it. **AC-D5** Success reports host + release id + verified remote sha + gate status. **AC-D6** Host-key card shows first-contact vs pinned honestly; prod first-contact requires fingerprint confirmation. **AC-D7** Post-transfer remote-sha mismatch hard-aborts and rolls back. **AC-D8 (headless)** With no operator channel, RequiresApproval/prod fail closed unless an explicit audited token is supplied.

**Review (§6):** **AC-R1** Every diff renders with gutters, hunk headers, collapse (read-only in MVP). **AC-R2** Findings inbox supports open/fix/accept/dismiss; fix re-runs audit; dismiss dims without accepting risk. **AC-R3** *(Post-MVP 4.5)* `Ctrl+E`/`E` opens a full-screen editor; save re-runs audit and refreshes findings. **AC-R4** Governance changes are visible in the diff and reflected in audit. **AC-R5** On a **prod** target, critical/high blocking findings cannot be cleared by `accept` — only by `fix`+passing re-audit.

**Audit/fix (§5):** **AC-F1** Deterministic gate failures surface into the inbox with file:line. **AC-F2** fix re-runs audit and reports the before/after gate delta. **AC-F3** The agent never claims "fixed" without a passing re-audit.

**Visual/interaction (§3,§4):** **AC-U1** magenta appears only on approval gates. **AC-U2** The status bar always shows model · git · posture · working indicator + `esc to interrupt` during a turn. **AC-U3** `Esc` interrupts cleanly preserving partial output; `Ctrl+C` is two-stage; mid-side-effect `Esc` maps to rollback, not raw cancel. **AC-U4** Posture (Shift+Tab) narrows auto-pass but never weakens the RequiresApproval/prod floor. **AC-U5** `Enter`-steer is disabled during approval and side-effecting execution; gate-deny takes precedence over stream-interrupt.
