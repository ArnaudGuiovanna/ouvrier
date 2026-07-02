# Ouvrier IDE — Implementation-Ready Spec (Quiet / Catppuccin Macchiato)

**Status:** Approved for build. Phase 1 (the spine) is the committed first slice. Produced by a product-design panel (designer-lead, UX, staff engineer, dev-experience) + adversarial critique; all seams and the load-bearing substrate claims (lipgloss/v2 compositor, textarea wrap behavior, `View() tea.View`, gopls at `/home/ubuntu/go/bin/gopls`) re-verified against the live tree on 2026-06-14 (go1.25.6, bubbles/bubbletea/lipgloss v2).

A sober terminal IDE to review, edit, save, and rebuild Ouvrier worker code — gopls diagnostics plus Ouvrier API snippets — opened by `ouvrier ide` on the current project. Hover, completion, and go-to-definition are layered in after the spine ships.

---

## 1. Product definition & the review loop

**Thesis.** The Ouvrier IDE is not a general editor — it is a *review surface* for code the agent already wrote. The operator opens the worker, reads it with two sets of eyes (gopls + Ouvrier-framework), makes the small correction the agent missed, saves, and lets the existing audit/build loop confirm. The IDE *demotes* everything that isn't the decision: calm by default, one accent, push-not-poll feedback, no UI ever blocking on the LSP.

**The loop (the product's spine):**

```
open worker → review (LSP diags + audit findings + review findings, one panel)
            → edit (textarea) → ctrl+s save → re-audit → finding vanishes
            → ctrl+b build → BuildArtifact sha in status line
```

The emotional payoff is the finding *disappearing* after a fix. Diagnostics merge from three sources, all keyed by `file:line`: LSP `publishDiagnostics` (live), `AuditReport.Results` (on save), `ReviewReport.Findings` (the agent's review the operator came to act on).

**First-slice honesty.** Slice 1 surfaces diagnostics as **gutter markers + a Problems panel**, both keyed by logical line — correct on every line including accented French. **Inline underlines are deferred to Phase 3**, because stock `textarea` always soft-wraps (`memoizedWrap` at `textarea.go:1370`) and exposes display-row geometry only for the cursor's line (`LineInfo()` at `textarea.go:1025`). Positioning an underline on an arbitrary off-cursor line would require re-implementing textarea's private wrap math. Gutter-markers are honest and complete for the review loop.

---

## 2. Entry point + project discovery

**Decision: one `ide.Model`, two launchers, asymmetric save behavior.**

1. **`ouvrier ide [path]`** — canonical. New `case "ide"` in the dispatch switch at `internal/cli/app.go:152` (beside `case "operate"`), backed by a `runIDE RunIDEFunc` field on `App` defaulting to `defaultRunIDE` (mirrors `runOperate`/`defaultRunOperate` so tests stub Bubble Tea). New `internal/cli/ide.go` parses `--dir`, discovers the project, launches `tui.RunIDE(ctx, in, out, IDEOptions{Workspace, GoplsPath})`.
2. **Cockpit `ctrl+g`** (and `/ide`) — promotes the IDE inside the live cockpit on `m.workspace`, via a `showIDE` flag paralleling `showEditor`. The existing `ctrl+e` single-pane textarea stays as lightweight quick-edit; `ctrl+g` is the full IDE.

**Save behavior differs by launcher:**
- Standalone: `ctrl+s` → `WriteWorkerFile` → `lsp.DidSave` → **direct** `operate.NewAuditRunner().Run(ctx, ws.Dir)` (tight deterministic cycle).
- Nested: `ctrl+s` → `WriteWorkerFile` → routes through the cockpit's `submit("audit the worker")` so the re-audit lands in the transcript. **Busy-agent rule:** if mid-stream, write the file, show `saved — audit queued (agent busy)`, and enqueue a single coalesced audit that fires when idle (later saves replace the pending request).

**Project discovery.** `operate.DetectWorkspace(dir)`; on error fall back to `detectOperateCandidates(dir)`; final failure: `no Ouvrier worker here (need pip.yaml + main.go)`. The **gopls module root** is resolved separately by walking up to the nearest `go.mod`/`go.work`; none → degraded mode with a one-line warning.

---

## 3. Layout & Catppuccin-Macchiato theme

### 3a. Token table (rewrite of `internal/tui/colors.go`) — value-repoint the hue-named consts to Macchiato so the ≈80 refs keep compiling, then add `internal/tui/theme.go` semantic aliases.

```
surfaces:  bgCrust #181926 · bgMantle #1e2030 · bgBase #24273a · surface0 #363a4f · surface1 #494d64 · surface2 #5b6078
text:      text #cad3f5 · subtext1 #b8c0e0 · subtext0 #a5adcb · overlay2 #939ab7 · overlay1 #8087a2 · overlay0 #6e738d (RULES ONLY)
accent:    accent #c6a0f6 (mauve, single accent) · accentDim #494d64 · link #8aadf4 (blue, structural secondary)
lifecycle: ok #a6da95 (success ONLY) · running #7dc4e4 (sapphire) · fail #ed8796 · gate #eed49f (yellow)
diag:      error #ed8796 · warn #eed49f · info #8aadf4 · hint #8bd5ca
diff:      add #a6da95 · del #ed8796
bg/fg vars: background #24273a · foreground #cad3f5
```

**Old→new repoint:** `blackHex`→#24273a, `offWhiteHex`→#cad3f5, `greenHex`→#a6da95 (ok), `mutedHex`→#939ab7, `cyanHex`→#7dc4e4, `yellowHex`→#eed49f, `redHex`→#ed8796, `dimGreenHex`→#494d64, `magentaHex`→#c6a0f6.

**Two deliberate moves:** green is **success only** — the "working" spinner moves to sapphire (`runningHex`) so the screen is never green-at-rest; the approval gate moves off magenta to **yellow** (`gateHex`). Accent mauve marks exactly three concepts: focus (cursor/selected row), identity (tool/agent name), Ouvrier-ness (the `◇` snippet glyph).

### 3b–3c. IDE token usage + ASCII mockups (representative)

**Mockup 1 — IDE with a diagnostic (gutter `●` + Problems panel):**
```
 worker/ ▾         │  7 │         Pipe(classer).                           │ Ouvrier API ◇
  ● main.go        │  8 │         Tool(notifier).                          │ ◇ From(trigger)
   ●               │  9 │         Reply(ovr.Accepted())                    │ ◇ Pipe(fn)
 ──────────────────┴────┴──────────────────────────────────────────────────┴────────────────
  Problems (1)
   ● main.go:8:9   error   undefined: notifier                              (compiler)
 ─────────────────────────────────────────────────────────────────────────────────────────
  REVIEW  worker/main.go   Ln 8, Col 9   gopls●   ●modified   ctrl+s save&audit · ctrl+b build
```
**Mockup 3 — Save → rebuild (the one green-dominant screen):**
```
  Build — passed
   ✓ gofmt   ✓ go vet   ✓ go test (3)   ✓ static build
   ✓ artifact  sha256:9f3c…a012   go1.25.1   audit-passed=true   git:7f02ce9
  REVIEW  worker/main.go   built ✓        esc → cockpit · ctrl+e reopen
```

### 3d. Theme-migration decision

A Go `const` has one value, so the repoint can't make `greenHex` mean both accent and success. **Do the full inline semantic migration of `greenHex`/`magentaHex`/`cyanHex` call sites in Phase 1** (~80 mechanical sites under `internal/tui/`), routing each to the correct alias (`accentHex` focus/identity, `okHex` success, `runningHex` live, `linkHex` hints). Acceptance criterion 2 ("green only for success") cannot pass otherwise. Repoint-then-migrate lands as one always-green-building commit.

### 3e. Contrast guards

`overlay0Hex #6e738d` ≈ 2.6:1 — **forbidden for any comprehension-bearing text** (incl. comments → use `overlay1Hex` ≈3.6:1); reserved for rules/separators. `diagInfoHex == linkHex` is intentional but avoided in slice 1 (no inline underlines).

---

## 4. Interaction model (full keybindings)

**Modeless.** Three regions cycle with `tab`/`shift+tab`; focused region carries a 1-cell mauve left-edge marker. One dimmed footer keymap.

- `esc` = dismiss topmost transient only — **never quit**. `ctrl+q` quits (clean LSP shutdown); `ctrl+c` force-quits.
- Save+rebuild `ctrl+s`; save-only `ctrl+w` (fallback for `ctrl+shift+s`); build `ctrl+b`.
- Next/prev diagnostic `]d`/`[d` (primary; `F8`/`shift+F8` where reported).
- Completion `ctrl+space` (or `.`) / accept `tab`|`enter` / cancel `esc` (Phase 2). Snippet palette `ctrl+p` (works without gopls). API panel `ctrl+\`.
- Apply finding `Action` = `ctrl+.` (editor) / `enter`|`a` (Problems) — **never bare `.`** (typed in Go).
- Hover `ctrl+k`, go-to-def `ctrl+]`, jump-back `ctrl+t` (Phase 3). Toggle Problems `ctrl+j`. Help `?`/`ctrl+/`. Undo/redo `ctrl+z`/`ctrl+y`.

**Quietness budget (all LSP I/O off the UI goroutine → `tea.Msg`):** `didChange` debounce 150 ms; auto-complete on `.` 120 ms; diagnostics on push only; stale hover/completion cancelled via `$/cancelRequest`; build/audit async with status streaming.

---

## 5. gopls LSP architecture (`internal/lsp`)

**Decision: hand-roll a ~300-line JSON-RPC 2.0 client over `os/exec` + stdlib. Do NOT vendor `go.lsp.dev/protocol`** (stale; canonical gopls protocol is `internal/`). Method set ≤6; structs hand-written to wire shapes. Zero Bubble Tea imports → unit-testable with a fake stdio server.

**Files:** `client.go` (lifecycle + dispatcher), `transport.go` (`Content-Length` framing), `protocol.go` (~15 structs), `position.go` (encoding conversion), `client_test.go` (fake server + French fixture).

**Public surface:**
```go
func New(ctx, goplsPath, rootDir string) (*Client, error)   // spawn + handshake (bounded)
func (c *Client) DidOpen(uri, text string)
func (c *Client) DidChange(uri, text string)                 // full-doc sync (MVP)
func (c *Client) DidSave(uri string)
func (c *Client) Diagnostics() <-chan PublishDiagnosticsParams // push
func (c *Client) Hover(ctx, uri string, pos Position) (*Hover, error)
func (c *Client) Complete(ctx, uri string, pos Position) (*CompletionList, error)
func (c *Client) Definition(ctx, uri string, pos Position) ([]Location, error)
func (c *Client) Encoding() PositionEncoding
func (c *Client) Shutdown(ctx) error
```

**Lifecycle.** `exec.CommandContext(ctx, gopls)` (no args = stdio server); StdinPipe (write-mutex'd), single StdoutPipe reader goroutine, stderr → ring buffer. Framing `Content-Length: N\r\n\r\n` + N bytes UTF-8 JSON; read headers via `bufio.Reader` then `io.ReadFull`. Dispatcher routes by `id`/`method`: monotonic id, `map[id]chan resp`. Handshake `initialize` → cache caps → `initialized` → legal to `didOpen`. Teardown `shutdown`→`exit`→`Wait(timeout)`→`Kill`; root-ctx cancel guarantees teardown.

**Handshake must not block launch.** The editor opens before the handshake; the client is spawned concurrently; diagnostics wire in once `initialized` lands. `New()` puts a bounded `context.WithTimeout` (default 12 s) on the `initialize` round-trip → on timeout, degraded mode with `lsp: slow to start — degraded · press r to retry`. Cold-start indexing surfaces as a sapphire `lsp ◐ indexing…` via `workDoneProgress`.

**MVP message set:** diagnostics push (`didOpen`/debounced `didChange` full-doc/`didSave` → `publishDiagnostics`; `didClose`). Completion (Phase 2): `textDocument/completion` → apply `TextEdit`/`InsertText` + `AdditionalTextEdits` (auto-imports). Hover/Definition Phase 3.

**InitializeParams:** `processId`, `rootUri`=module root, `workspaceFolders`, `general.positionEncodings:["utf-8","utf-16"]`, `completion.completionItem.{snippetSupport,resolveSupport}`, `hover.contentFormat:["markdown","plaintext"]`, `window.workDoneProgress:true`; `initializationOptions:{ui.diagnostic.staticcheck:true, completeUnimported:true, usePlaceholders:true}`.

**Three correctness traps:**
1. **Position encoding** — request UTF-8 but **read back** `InitializeResult.capabilities.positionEncoding` and store it. Converter `Convert(doc []string, pos Position) (row, runeCol int)` holds the line buffer (LSP `character` is code units into that line's text). Clamp empty file / past-EOL / past-EOF; ranges half-open. **Committed fixture:** `client_test.go` asserts a multi-byte line (`é` at col 5, real French worker strings) round-trips under both `utf-8` and `utf-16`. The slice does not ship without it.
2. **Server→client requests** — answer `workspace/configuration` (`[settings]`), `window/workDoneProgress/create` (`null`), `client/registerCapability` (`null`) or gopls stalls; `publishDiagnostics` is a notification.
3. **File URIs** — `(&url.URL{Scheme:"file", Path: filepath.ToSlash(abs)}).String()`; canonical URI↔buffer map.

**Fallback.** Discovery `exec.LookPath("gopls")` → `$GOBIN/gopls` → `$(go env GOPATH)/bin/gopls`. None → edit/save/build/audit+snippets mode; hide hover/completion/definition; calm `subtext0` line `gopls not found — go install golang.org/x/tools/gopls@latest · press r to retry`. Mid-session EOF → `lsp: disconnected`, one backoff restart, then degraded.

---

## 6. Editor component + compositing + snippet engine + API panel

**6a. Wrap `charm.land/bubbles/v2/textarea`; do NOT build a custom buffer.** It does line numbers, multiline edit, selection, soft-wrap, h-scroll, and is the proven `/edit` seam. Overlay chrome in `view.go`: recolored gutter/line-numbers; **diagnostic gutter markers** (●/■/⚠/◦ per severity, highest wins per line) keyed by **logical line** (maps trivially); file tree walks `Workspace.Dir` reusing the `goFiles` skip-set (`audit.go:218`); layout via `lipgloss.JoinHorizontal/JoinVertical`; right+bottom panels collapsible (default: editor + Problems only).

**Inline underlines are Phase 3** (gated on width-forcing layout math, not textarea internals).

**6b. Compositing.** `View()` returns `tea.View` (struct), verified at `operate.go:959`. Popups/completion/hover are rendered with **lipgloss/v2's compositor — `lipgloss.NewLayer(content).X().Y().Z()` onto a `*lipgloss.Canvas`** (verified `lipgloss/v2@v2.0.2/layer.go`,`canvas.go`). No bespoke string-slicing overlay. Adopt the Layer/Canvas base from Phase 1 even before popups exist.

**6c. Snippet engine (`internal/operate/snippets`)** — `//go:embed pack/*` (precedent `internal/console/assets.go:10`), no TUI/LSP imports. API `Snippets() []Snippet{Prefix,Title,Body,Doc,Group}`, `Search(q)`, `Primitive(name)`. ~26 snippets, 5 groups (trigger/pipeline/governance/reply), bodies with `${1:}` tab stops, sourced from `trigger.go`/`pipeline.go`/`options.go`/`output.go`/`tool.go` + the API pack. Phase 2 merges gopls + Ouvrier snippets (mauve `◇`, high `SortText`); `ctrl+p` palette works in fallback. **Refactor `toolReadAPI`/`toolSearchDocs` (`tools.go:136-172`) to delegate to this pack** (fixes a live bug: `toolSearchDocs` returns zero matches outside the framework checkout).

**6d. Ouvrier API panel (`ctrl+\`)** — same pack: Browse / Context (cursor on a primitive auto-scrolls to its doc) / Search.

---

## 7. Save → audit/build loop (reuse)

- `ctrl+s` → `operate.WriteWorkerFile` (keeps `safeWorkerPath` sandbox) → `lsp.DidSave` → standalone `operate.NewAuditRunner().Run(ctx, ws.Dir)` (`audit.go:68`) → render `report.Results []GateResult` into Problems; nested via cockpit `submit` + busy coalescing.
- `ctrl+b` → `BuildCoordinator{}.Build(ctx, sessionID, ws.Dir, target, auditPassed, progress)` (`build.go:39`) → `BuildArtifact` (sha/go-version/git-rev) in status line; ok green / running sapphire / fail red.
- Save rejected by sandbox → red status, nothing written.

**Unified Problems panel — three sources, one keyed model**, sorted severity then `file:line`, faint source tag (`review`/`audit`/`lsp`), identical severity colors. **`severityGlyph` (`operate_view.go:693`) is extended, not replaced.**

---

## 8. New packages & files + reuse map

**Create:** `internal/lsp/{client,transport,protocol,position,client_test}.go` · `internal/tui/ide/{model,view,tree,problems,apipanel,completion,popup,position}.go` · `internal/tui/theme.go` · `internal/operate/snippets/{snippets.go,search.go,pack/*}` · `internal/cli/ide.go`.

**Modify:** `internal/tui/colors.go` (repoint + semantic migration) · `internal/cli/app.go` (`case "ide"` at :152; `runIDE`/`defaultRunIDE`) · `internal/tui/operate.go` (`ctrl+g`/`/ide` nested launch; keep `ctrl+e`) · `internal/operate/tools.go:136-172` (delegate to snippets) · `internal/cli/help.go` (doc `ide`).

**Reuse (verified):** `workerfile.go:11-43` · `audit.go:68,218` · `build.go:39` · `review.go:28-184` · `workspace.go:38` · `operate_view.go:693,706` · `cli/operate.go` seam · `console/assets.go:10` embed · `lipgloss/v2 layer.go/canvas.go`.

---

## 9. Phased vertical-slice build plan

- **Phase 1 — Open + diagnostics + save + rebuild (the spine).** `internal/lsp` full client (spawn, framing, JSON-RPC, initialize/initialized/shutdown, **encoding negotiate+verify with committed French fixture**, **bounded initialize timeout**, server→client replies, fallback). `theme.go` + repoint `colors.go` + **inline semantic migration**. `internal/tui/ide`: tree + textarea + Problems, **`View()` returning `tea.View`**, **lipgloss Layer/Canvas compositor from day one**. Editor opens before handshake; diagnostics wire on `initialized`. `didOpen` + debounced full-sync `didChange` → **gutter markers + Problems** (no inline underlines). `ctrl+s` → `WriteWorkerFile` + `AuditRunner`. `ctrl+b` → `BuildCoordinator`. `ouvrier ide` command. gopls-missing + slow-start fallback. Unit tests (fake stdio gopls + fake workspace).
- **Phase 2 — Completion + snippets.** `internal/operate/snippets` pack; refactor `toolReadAPI`/`toolSearchDocs`. `textDocument/completion` popup (Layer) with `AdditionalTextEdits`. Merge gopls + Ouvrier snippets. `ctrl+p` palette + `ctrl+\` API panel (work in fallback). `$/cancelRequest`.
- **Phase 3 — Hover + definition + inline underlines + polish.** `textDocument/hover` popover; `textDocument/definition` nav; inline underlines via width-forcing if asked; cockpit `ctrl+g`; `ctrl+.` apply-finding.
- **Phase 4 — Optional.** Incremental sync; code actions/quick-fixes; references; rename.

---

## 10. Risks & non-goals

**Risks (+ mitigation):** position-encoding on French strings → verify negotiated encoding + `Convert(doc,pos)` + committed fixture; gopls cold-start → bounded timeout → degraded, editor opens first, `workDoneProgress` spinner; gopls memory (~200 MB–1 GB) → deliberate fallback on constrained hosts; gopls stall → answer all server→client requests; gopls absent/crash → never block UI; theme regression (~80 refs) → repoint + Phase-1 migration; Catppuccin contrast → `overlay0` forbidden for text.

**Non-goals (v1):** general editing outside the worker sandbox; multi-file refactor/rename/references (Phase 4); full syntax theme (≤4 hues); split panes/multi-cursor; remote/non-Go LSPs; mouse-first; second accent; LSP pull diagnostics; inline underlines in slice 1.

---

## 11. Acceptance criteria

1. **Entry & discovery.** `ouvrier ide` in a worker dir opens the IDE; outside → `detectOperateCandidates` then exit with `no Ouvrier worker here…`. Cockpit `ctrl+g`/`/ide` opens the same model; `ctrl+e` still opens the legacy quick-edit.
2. **Theme.** No `#00d27a`/`#0a0a0a`/`#fafafa` rendered; background `#24273a`, text `#cad3f5`; mauve only on cursor/selected-row/`◇`; **green only for success (after the inline migration)**; spinner sapphire; gate yellow; no comprehension text on `overlay0`.
3. **Diagnostics (slice-1 form).** Editing introduces an error → within ~150 ms a red gutter `●` on the correct logical line + a Problems row; logical-line mapping correct including accented French (committed fixture).
4. **Editor opens before LSP.** Tree+editor render immediately even when gopls is slow/absent; diagnostics appear on `initialized`; >12 s handshake → degraded with a calm status, never a blank held screen.
5. **Completion + snippets (Phase 2).** `ctrl+space`/`.` popup (Layer); Ouvrier primitives sort to top with mauve `◇`; `tab` accepts + applies auto-imports; `ctrl+p` palette inserts a snippet with `${1:}` stops.
6. **Hover (Phase 3).** `ctrl+k` shows a Macchiato Layer popover; cursor move dismisses + cancels in-flight.
7. **Save→audit loop.** `ctrl+s` writes via `WriteWorkerFile` (sandbox blocks abs/`..`/`.git`, red status, no write), triggers re-audit, Problems repopulates from `GateResult`; a fixed finding disappears. Standalone direct `AuditRunner`; nested via `submit` with busy coalescing.
8. **Build.** `ctrl+b` runs `BuildCoordinator.Build`; success → `built ✓ <sha>` + green block; failure → red `✗`.
9. **gopls fallback.** Uninstalled gopls → IDE opens; edit/save/audit/build/snippets/API-panel work; hover/completion/definition hidden; calm install hint; `r` retries.
10. **Quietness & bindings.** No toasts; dimmed footer keymap; `esc` only dismisses transients; `ctrl+q` clean quit, `ctrl+c` force-quit even if gopls hung; apply-action `ctrl+.` (never bare `.`); `]d`/`[d` diagnostic nav; `ctrl+w` save-only.
11. **Testability.** `internal/lsp` passes unit tests vs a fake stdio gopls (framing, handshake, **encoding verify with French fixture under utf-8 and utf-16**, diagnostics routing); `internal/operate/snippets` has no TUI/LSP imports; `runIDE` stubbable like `runOperate`.
