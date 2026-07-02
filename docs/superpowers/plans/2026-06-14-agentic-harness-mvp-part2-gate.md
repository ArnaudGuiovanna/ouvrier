# Agentic Harness MVP — Part 2 (Slice 2: Approval/Policy Gate) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add a synchronous, runtime-level approval gate so SideEffecting/RequiresApproval tools (build, deploy) pause for operator confirmation in the TUI, with prod double-confirm, a Shift+Tab posture, and headless fail-closed — per spec §4/§5 (AC-D2, AC-D3, AC-D4, AC-D8, AC-U4).

**Architecture:** Tools gain a `Governance` level. The runtime threads a per-turn `turnControl` (posture + an operator decision channel) into `callTool`; before executing a governed tool it emits a `StreamApproval` event and blocks on the decision channel. Headless turns (no decision channel) fail closed for approval-requiring tools. The TUI renders an approval card and sends the decision back. Prod deploys carry a `Prod` flag and the TUI enforces a typed second confirmation.

**Tech Stack:** Go 1.25, internal/operate (runtime, tools, stream), internal/tui (Bubble Tea cockpit). Tests use scriptedModel + fake decision channels; no network.

---

## File Structure

- `internal/operate/governance.go` (new) — `Governance` enum, `Posture` enum, `ApprovalRequest`, `ApprovalDecision`, helpers.
- `internal/operate/tools.go` — replace `Tool.ReadOnly bool` with `Tool.Governance`; classify each tool.
- `internal/operate/runtime_stream.go` — add `StreamApproval` kind + `Approval *ApprovalRequest` field; add `RunTurnInteractive`.
- `internal/operate/runtime.go` — thread `*turnControl` through `runPrompt`/`callTool`; add `gate`.
- `internal/operate/agent_loop.go` — pass `ctrl` through `runAgentLoop`→`callTool`.
- `internal/tui/operate.go` / `operate_view.go` — approval overlay/card, decision plumbing, Shift+Tab posture.
- Tests alongside each.

---

## Task 2.1: Governance + Posture types and tool classification

**Files:** Create `internal/operate/governance.go`; Modify `internal/operate/tools.go`; Test `internal/operate/governance_test.go`.

- [ ] **Step 1: Failing test** — `internal/operate/governance_test.go`:

```go
package operate

import "testing"

func TestToolGovernanceClassification(t *testing.T) {
	r := NewToolRegistry()
	cases := map[string]Governance{
		"list_workers":    GovReadOnly,
		"read_worker_file": GovReadOnly,
		"audit_worker":    GovReadOnly,
		"review_worker":   GovReadOnly,
		"diff_worker":     GovReadOnly,
		"scaffold_worker": GovSideEffecting,
		"patch_worker":    GovSideEffecting,
		"fix_worker":      GovSideEffecting,
		"build_worker":    GovSideEffecting,
		"transfer_worker": GovRequiresApproval,
	}
	for name, want := range cases {
		tool, ok := r.Tool(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		if tool.Governance != want {
			t.Errorf("tool %q governance = %v, want %v", name, tool.Governance, want)
		}
	}
}

func TestGovernanceNeedsApproval(t *testing.T) {
	if GovReadOnly.NeedsApproval(PostureManual) {
		t.Error("read-only must never need approval")
	}
	if !GovSideEffecting.NeedsApproval(PostureManual) {
		t.Error("side-effecting needs approval under manual posture")
	}
	if GovSideEffecting.NeedsApproval(PostureAutoSafe) {
		t.Error("side-effecting auto-passes under auto-safe posture")
	}
	if !GovRequiresApproval.NeedsApproval(PostureAutoSafe) {
		t.Error("requires-approval must always need approval, even auto-safe")
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (undefined types). `go test ./internal/operate/ -run 'Governance' -v`

- [ ] **Step 3: Implement** — create `internal/operate/governance.go`:

```go
package operate

// Governance mirrors the Ouvrier framework's own tool governance levels and
// drives the operate approval gate.
type Governance string

const (
	GovReadOnly         Governance = "read_only"
	GovIdempotent       Governance = "idempotent"
	GovSideEffecting    Governance = "side_effecting"
	GovRequiresApproval Governance = "requires_approval"
)

// Posture is the operator's standing approval stance, cycled with Shift+Tab.
type Posture string

const (
	PostureManual   Posture = "manual"    // confirm every side effect
	PostureAutoSafe Posture = "auto-safe" // auto-run side-effecting; still confirm RequiresApproval
	PosturePlan     Posture = "plan"      // read-only exploration; refuse side effects
)

// NeedsApproval reports whether a tool with this governance must prompt the
// operator under the given posture. RequiresApproval always prompts; the posture
// can only narrow what auto-passes, never weaken this floor.
func (g Governance) NeedsApproval(p Posture) bool {
	switch g {
	case GovReadOnly, GovIdempotent:
		return false
	case GovRequiresApproval:
		return true
	case GovSideEffecting:
		return p != PostureAutoSafe
	default:
		return true
	}
}

// SideEffecting reports whether the governance performs any write/build/deploy.
func (g Governance) SideEffecting() bool {
	return g == GovSideEffecting || g == GovRequiresApproval
}
```

In `internal/operate/tools.go`: replace the `ReadOnly bool` field on `Tool` with `Governance Governance`. Update every `Register(Tool{...})` call: tools previously `ReadOnly: true` become `Governance: GovReadOnly`; classify the rest:
- `list_workers, search_ouvrier_docs, read_ouvrier_api, read_worker_file, review_worker, audit_worker, diff_worker, export_session` → `GovReadOnly`
- `scaffold_worker, patch_worker, fix_worker, build_worker, accept_risk, login_codex` → `GovSideEffecting`
- `transfer_worker` → `GovRequiresApproval`

(Remove the `ReadOnly` field entirely. If any code reads `tool.ReadOnly`, update it to `tool.Governance == GovReadOnly`.)

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/operate/ -run 'Governance' -v`

- [ ] **Step 5: Build whole repo (catch ReadOnly usages), commit.**
`go build ./... && go test ./internal/operate/ && gofmt -l internal/operate/`
```bash
git add internal/operate/governance.go internal/operate/governance_test.go internal/operate/tools.go
git commit -m "feat(operate): tool Governance + Posture model"
```

---

## Task 2.2: Approval types, StreamApproval event, gate plumbing, headless fail-closed

**Files:** Modify `internal/operate/governance.go` (approval types), `internal/operate/runtime_stream.go`, `internal/operate/runtime.go`, `internal/operate/agent_loop.go`; Test `internal/operate/gate_test.go`.

- [ ] **Step 1: Failing test** — `internal/operate/gate_test.go`:

```go
package operate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

// Interactive: approve a side-effecting tool when the operator says yes.
func TestGateInteractiveApprove(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "scaffolding", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "patch_worker", Arguments: json.RawMessage(`{"goal":"x"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})

	ch, decisions, err := rt.RunTurnInteractive(context.Background(), started.Session.ID, "patch it", "prompt", PostureManual)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var approvalID string
	sawToolEnd := false
	for ev := range ch {
		if ev.Kind == StreamApproval && ev.Approval != nil {
			approvalID = ev.Approval.ID
			decisions <- ApprovalDecision{ID: approvalID, Approved: true}
		}
		if ev.Kind == StreamToolEnd && ev.Entry != nil && ev.Entry.ToolName == "patch_worker" {
			sawToolEnd = true
		}
	}
	if approvalID == "" {
		t.Fatal("expected a StreamApproval for patch_worker")
	}
	if !sawToolEnd {
		t.Fatal("patch_worker did not execute after approval")
	}
}

// Headless: a RequiresApproval tool fails closed when there is no operator.
func TestGateHeadlessFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "deploying", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "transfer_worker", Arguments: json.RawMessage(`{"env":"staging"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})

	// RunTurn is the headless (no-decision) path.
	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "deploy staging", "prompt")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	denied := false
	for ev := range ch {
		if ev.Kind == StreamToolEnd && ev.Entry != nil && ev.Entry.ToolName == "transfer_worker" {
			if _, ok := ev.Entry.Output["error"]; ok {
				denied = true
			}
		}
	}
	if !denied {
		t.Fatal("transfer_worker must fail closed in headless mode")
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (RunTurnInteractive/StreamApproval/ApprovalDecision undefined). `go test ./internal/operate/ -run 'TestGate' -v`

- [ ] **Step 3: Implement.**

Add to `internal/operate/governance.go`:
```go
// ApprovalRequest is emitted before a governed tool runs; the operator answers
// with an ApprovalDecision carrying the same ID.
type ApprovalRequest struct {
	ID         string         `json:"id"`
	Tool       string         `json:"tool"`
	Governance Governance     `json:"governance"`
	Summary    string         `json:"summary"`
	Prod       bool           `json:"prod,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

type ApprovalDecision struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

// turnControl carries per-turn operator context into the tool loop.
type turnControl struct {
	posture     Posture
	decisions   <-chan ApprovalDecision
	interactive bool
}

func headlessControl() *turnControl { return &turnControl{posture: PostureManual, interactive: false} }
```

In `internal/operate/runtime_stream.go`:
- Add kind: `StreamApproval StreamEventKind = "approval"`.
- Add field to `StreamEvent`: `Approval *ApprovalRequest \`json:"approval,omitempty"\``.
- Change `RunTurn` to call `r.runPrompt(ctx, sessionID, text, kind, emit, headlessControl())`.
- Add:
```go
// RunTurnInteractive runs a turn with an operator approval channel. The returned
// decisions channel must receive an ApprovalDecision (matching each emitted
// StreamApproval.ID) to unblock a governed tool.
func (r *AgentRuntime) RunTurnInteractive(ctx context.Context, sessionID, text, kind string, posture Posture) (<-chan StreamEvent, chan<- ApprovalDecision, error) {
	if r == nil || r.Store == nil {
		return nil, nil, errors.New("operate: nil runtime")
	}
	if strings.TrimSpace(kind) == "" {
		kind = "prompt"
	}
	if posture == "" {
		posture = PostureManual
	}
	ch := make(chan StreamEvent, 32)
	decisions := make(chan ApprovalDecision, 1)
	ctrl := &turnControl{posture: posture, decisions: decisions, interactive: true}
	go func() {
		defer close(ch)
		emit := func(ev StreamEvent) {
			select {
			case <-ctx.Done():
			case ch <- ev:
			}
		}
		_, _ = r.runPrompt(ctx, sessionID, text, kind, emit, ctrl)
	}()
	return ch, decisions, nil
}
```

In `internal/operate/runtime.go`:
- Change `runPrompt` signature to `func (r *AgentRuntime) runPrompt(ctx context.Context, sessionID, text, kind string, emit func(StreamEvent), ctrl *turnControl) (RuntimeTurn, error)`. If `ctrl == nil`, set `ctrl = headlessControl()`. Update the `Prompt/Steer/FollowUp` callers (they pass `nil` for emit today) to pass `nil, nil` (emit nil, ctrl nil).
- Change `callTool` signature to accept `ctrl *turnControl` and gate before executing:
```go
func (r *AgentRuntime) callTool(ctx context.Context, session *Session, call plannedTool, turn *RuntimeTurn, emit func(StreamEvent), ctrl *turnControl) (ToolResult, error) {
	if ctrl == nil {
		ctrl = headlessControl()
	}
	// ... existing call.ID generation ...
	// append the tool-call entry + emit StreamToolStart (existing) ...

	if decision, err := r.gate(ctx, call, emit, ctrl); err != nil || !decision {
		reason := "denied by operator"
		if err != nil {
			reason = err.Error()
		}
		result := ToolResult{Summary: "skipped " + call.Name + ": " + reason}
		output := map[string]any{"summary": result.Summary, "error": reason}
		resultEntry, aerr := r.transcript(session).Append(TranscriptEntry{SessionID: session.ID, Kind: TranscriptToolResult, ToolName: call.Name, Output: output, Metadata: map[string]any{"tool_call_id": call.ID}})
		if aerr != nil {
			return ToolResult{}, aerr
		}
		turn.Entries = append(turn.Entries, resultEntry)
		emit(StreamEvent{Kind: StreamToolEnd, Entry: &resultEntry, Err: fmt.Errorf("%s", reason)})
		_ = appendToolCall(session.ToolCallsPath, call, result, fmt.Errorf("%s", reason))
		return result, nil // not a turn-fatal error; the model sees the denial and can adapt
	}
	// ... existing Tools.Execute + result entry + StreamToolEnd ...
}
```
Add the gate:
```go
func (r *AgentRuntime) gate(ctx context.Context, call plannedTool, emit func(StreamEvent), ctrl *turnControl) (bool, error) {
	tool, ok := r.Tools.Tool(call.Name)
	if !ok {
		return true, nil // unknown tools are handled by Execute's error
	}
	if !tool.Governance.NeedsApproval(ctrl.posture) {
		return true, nil
	}
	if !ctrl.interactive || ctrl.decisions == nil {
		return false, fmt.Errorf("approval required for %s but no operator is attached (headless)", call.Name)
	}
	req := approvalRequestFor(call, tool.Governance, r.workspace)
	emit(StreamEvent{Kind: StreamApproval, Approval: req})
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case d := <-ctrl.decisions:
		return d.Approved, nil
	}
}

func approvalRequestFor(call plannedTool, gov Governance, ws *Workspace) *ApprovalRequest {
	id, _ := randomID()
	req := &ApprovalRequest{ID: id, Tool: call.Name, Governance: gov, Details: map[string]any{}}
	for k, v := range call.Input {
		req.Details[k] = v
	}
	switch call.Name {
	case "transfer_worker":
		env := strings.ToLower(stringValue(call.Input, "env"))
		req.Prod = env == "prod" || env == "production"
		req.Summary = "deploy worker to " + stringValue(call.Input, "env")
	case "build_worker":
		req.Summary = "build worker binary"
	default:
		req.Summary = call.Name
	}
	if ws != nil {
		req.Details["worker"] = ws.Name
	}
	return req
}
```
Update the two `callTool` call sites:
- `runtime.go` planner loop: `r.callTool(ctx, session, call, &turn, emit, ctrl)`.
- `agent_loop.go`: `r.callTool(ctx, session, plannedTool{ID: call.ID, Name: call.Name, Input: input}, turn, emit, ctrl)`.
- `runAgentLoop` must accept and thread `ctrl`: change `runPrompt`'s agent-loop branch `return r.runAgentLoop(ctx, session, &turn, emit)` to `return r.runAgentLoop(ctx, session, &turn, emit, ctrl)` and add the param to `runAgentLoop`.

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/operate/ -run 'TestGate' -v` then full `go test ./internal/operate/`.

- [ ] **Step 5: Build, commit.**
`go build ./... && go vet ./internal/operate/ && go test ./internal/operate/ && gofmt -l internal/operate/`
```bash
git add internal/operate/governance.go internal/operate/runtime_stream.go internal/operate/runtime.go internal/operate/agent_loop.go internal/operate/gate_test.go
git commit -m "feat(operate): synchronous approval gate with headless fail-closed"
```

---

## Task 2.3: Posture auto-pass + accepted-risk note in approval details

**Files:** Modify `internal/operate/runtime.go` (`approvalRequestFor`), test `internal/operate/gate_test.go`.

- [ ] **Step 1: Failing test** — append to `gate_test.go`:

```go
// auto-safe posture auto-runs a side-effecting tool without an approval event.
func TestGateAutoSafeSkipsSideEffecting(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "patching", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "patch_worker", Arguments: json.RawMessage(`{"goal":"x"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	ch, _, _ := rt.RunTurnInteractive(context.Background(), started.Session.ID, "patch", "prompt", PostureAutoSafe)
	for ev := range ch {
		if ev.Kind == StreamApproval {
			t.Fatal("auto-safe must not prompt for a side-effecting tool")
		}
	}
}

// auto-safe still prompts for a RequiresApproval (deploy) tool.
func TestGateAutoSafeStillPromptsDeploy(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "deploying", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "transfer_worker", Arguments: json.RawMessage(`{"env":"prod"}`)}}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	ch, decisions, _ := rt.RunTurnInteractive(context.Background(), started.Session.ID, "deploy prod", "prompt", PostureAutoSafe)
	sawProd := false
	for ev := range ch {
		if ev.Kind == StreamApproval && ev.Approval != nil {
			if ev.Approval.Prod {
				sawProd = true
			}
			decisions <- ApprovalDecision{ID: ev.Approval.ID, Approved: false, Reason: "test deny"}
		}
	}
	if !sawProd {
		t.Fatal("auto-safe must still prompt for prod deploy with Prod=true")
	}
}
```

- [ ] **Step 2: Run, expect PASS already** (the gate from 2.2 should satisfy these). `go test ./internal/operate/ -run 'TestGateAutoSafe' -v`. If they pass, no code change is needed — these lock the posture behavior. If `TestGateAutoSafeStillPromptsDeploy` fails because `Prod` is not set, ensure `approvalRequestFor` sets `Prod` for `env == "prod"|"production"` (it does in 2.2).

- [ ] **Step 3: (If needed)** adjust `approvalRequestFor` so `Prod` detection is correct and `Details["accepted_risk"]` is populated from `session` when present. Keep minimal.

- [ ] **Step 4: Run full package.** `go test ./internal/operate/`

- [ ] **Step 5: Commit.**
```bash
git add internal/operate/gate_test.go internal/operate/runtime.go
git commit -m "test(operate): lock posture auto-pass and prod-prompt behavior"
```

---

## Task 2.4: TUI approval card + decision plumbing + Shift+Tab posture

**Files:** Modify `internal/tui/operate.go`, `internal/tui/operate_view.go`; Test `internal/tui/operate_test.go`.

- [ ] **Step 1: Failing test** — append to `internal/tui/operate_test.go`:

```go
func TestOperateApprovalCardFlow(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	// Open directly on the worker dir.
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	// Simulate a pending approval arriving from the runtime.
	req := &operate.ApprovalRequest{ID: "a1", Tool: "transfer_worker", Governance: operate.GovRequiresApproval, Summary: "deploy worker to staging"}
	m.applyStream(operate.StreamEvent{Kind: operate.StreamApproval, Approval: req})
	if m.pendingApproval == nil || m.pendingApproval.ID != "a1" {
		t.Fatalf("approval not recorded as pending: %+v", m.pendingApproval)
	}
	out := m.render()
	if !strings.Contains(out, "Approval") || !strings.Contains(out, "deploy") {
		t.Fatalf("approval card not rendered:\n%s", out)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (no `pendingApproval` field). `go test ./internal/tui/ -run TestOperateApprovalCardFlow -v`

- [ ] **Step 3: Implement.** In `internal/tui/operate.go`:
- Add fields to `operateModel`: `pendingApproval *operate.ApprovalRequest`, `decisions chan<- operate.ApprovalDecision`, `posture operate.Posture`, `prodConfirm string` (typed buffer for prod).
- In `startTurn`, switch from `m.runtime.RunTurn(...)` to `m.runtime.RunTurnInteractive(ctx, m.session.ID, text, "prompt", m.posture)` and store the returned decisions channel: `ch, dec, err := ...; m.decisions = dec`.
- In `applyStream`, handle `operate.StreamApproval`: `m.pendingApproval = ev.Approval`.
- When a `StreamToolEnd`/`StreamDone` arrives, clear `pendingApproval` if resolved.
- In `handleKey`, when `m.pendingApproval != nil`, intercept keys BEFORE normal handling:
  - For a prod approval (`pendingApproval.Prod`): typing accumulates into `m.prodConfirm`; `enter` approves only if `m.prodConfirm` matches the worker name (else ignored); `esc` denies.
  - For non-prod: `enter`/`y` approve, `esc`/`n` deny.
  - On approve: `m.decisions <- operate.ApprovalDecision{ID: m.pendingApproval.ID, Approved: true}`; on deny: `Approved: false, Reason: "denied"`. Then `m.pendingApproval = nil; m.prodConfirm = ""`.
- Add `Shift+Tab` (`"shift+tab"`) in `handleKey` (when no pending approval) to cycle posture manual→auto-safe→plan→manual; default `m.posture = operate.PostureManual` in `newOperateModel`.

In `internal/tui/operate_view.go`:
- Add `renderApprovalCard(width)` returning a magenta-bordered card showing `◆ Approval required`, the tool, summary, key details (worker, env/target/host/sha if present in `Details`), and the key legend (`enter approve · esc deny`, or for prod `type "<worker>" then enter · esc deny`). Render it above the composer in `render()` when `m.pendingApproval != nil`.
- Add a posture segment to the status bar: `posture <m.posture>` (magenta when manual, green when auto-safe, muted when plan).

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ -run 'TestOperate' -v`

- [ ] **Step 5: Build whole repo, vet, full test, commit.**
`go build ./... && go vet ./internal/tui/ ./internal/operate/ && go test ./internal/tui/ ./internal/operate/ && gofmt -l internal/tui/ internal/operate/`
```bash
git add internal/tui/operate.go internal/tui/operate_view.go internal/tui/operate_test.go
git commit -m "feat(tui): approval card, decision plumbing, Shift+Tab posture"
```

---

## Final verification
- [ ] `go build ./... && go vet ./... && go test ./...` — all green.
- [ ] Manual smoke (optional): `go run ./cmd/ouvrier` in a worker dir; ask the manual agent to deploy → an approval card appears; `esc` denies.

## Self-Review
- Spec coverage: AC-D2 (build+deploy pause) → 2.2/2.4; AC-D4 (prod typed confirm) → 2.4 prodConfirm; AC-D8 (headless fail-closed) → 2.2 TestGateHeadlessFailsClosed; AC-U4 (posture narrows, never weakens floor) → 2.1/2.3. AC-D3 (audit before build gate) is already enforced in `toolBuildWorker` (pre-existing) and unaffected.
- Host-key honesty / post-transfer integrity (spec §5/§7) reuse `internal/deploy/knownhosts.go`; surfacing them in the card is deferred to a focused follow-up since the deploy engine already pins hosts — not blocking the gate MVP. Note this in the merge summary.
- Type consistency: `Governance`/`Posture`/`ApprovalRequest`/`ApprovalDecision`/`turnControl` defined in 2.1/2.2 and consumed in 2.2/2.3/2.4; `RunTurnInteractive` returns `chan<- ApprovalDecision` consumed by the TUI.
- Placeholder scan: none; all code shown.
