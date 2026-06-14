# Agentic Harness MVP — Part 4 (Slice 4: Review IDE) Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps.

**Goal:** Render review output as a navigable findings inbox + read-only diff viewer in the cockpit, with `fix` routing a finding back to the agent (which re-audits) — per spec §6 (AC-R1, AC-R2, AC-F1, AC-F2).

**Architecture:** When `review_worker`/`diff_worker` complete, the runtime emits structured `StreamReview` / `StreamDiff` events parsed from the tool result. The TUI stores the latest findings + diff and shows a `review` overlay (toggled with Ctrl+R) with a findings list and a read-only diff pane; `f` on a finding submits a fix prompt (the existing agent path runs `fix_worker`, which re-audits). `accept`/`dismiss` set local finding state.

**Tech Stack:** Go 1.25; reuses `operate.Finding`, `review_worker`/`diff_worker` tool Data; Bubble Tea. Tests hermetic (scriptedModel + direct model calls).

---

## File Structure
- `internal/operate/runtime_stream.go` — `StreamReview`/`StreamDiff` kinds; `ReviewData`/`DiffData`; `StreamEvent.Review`/`.Diff` fields.
- `internal/operate/runtime.go` — in `callTool`, after a successful review/diff tool, emit the structured event (parse `result.Data`).
- `internal/tui/operate.go` / `operate_view.go` — review overlay state + rendering + keys.
- Tests alongside.

---

## Task 4.1: StreamReview / StreamDiff events from the runtime

**Files:** Modify `internal/operate/runtime_stream.go`, `internal/operate/runtime.go`; Test `internal/operate/review_events_test.go`.

- [ ] **Step 1: Failing test** — `internal/operate/review_events_test.go`:

```go
package operate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
)

func TestRunTurnEmitsReviewAndDiff(t *testing.T) {
	dir := t.TempDir()
	writeMinimalWorker(t, dir)
	model := &scriptedModel{steps: []provider.Response{
		{Text: "reviewing", StopReason: provider.StopToolUse,
			ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "review_worker", Arguments: json.RawMessage(`{"scope":"whole_worker"}`)},
				{ID: "c2", Name: "diff_worker", Arguments: json.RawMessage(`{}`)},
			}},
		{Text: "done", StopReason: provider.StopEndTurn},
	}}
	rt, _ := NewAgentRuntime(RuntimeOptions{Dir: dir, Driver: ManualDriver{}, Model: model, ModelID: "test/model"})
	started, _ := rt.Start(context.Background(), RuntimeStartRequest{Dir: dir})
	ch, err := rt.RunTurn(context.Background(), started.Session.ID, "review and diff", "prompt")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var sawReview, sawDiff bool
	for ev := range ch {
		if ev.Kind == StreamReview && ev.Review != nil {
			sawReview = true
		}
		if ev.Kind == StreamDiff && ev.Diff != nil {
			sawDiff = true
		}
	}
	if !sawReview {
		t.Fatal("expected a StreamReview event after review_worker")
	}
	if !sawDiff {
		t.Fatal("expected a StreamDiff event after diff_worker")
	}
}
```

(`review_worker` and `diff_worker` are GovReadOnly so they auto-pass the gate in headless RunTurn.)

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/operate/ -run TestRunTurnEmitsReviewAndDiff -v`

- [ ] **Step 3: Implement.**

In `internal/operate/runtime_stream.go` add:
```go
const (
	StreamReview StreamEventKind = "review"
	StreamDiff   StreamEventKind = "diff"
)

// ReviewData is the structured payload of a StreamReview event.
type ReviewData struct {
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// DiffData is the structured payload of a StreamDiff event.
type DiffData struct {
	Status       string   `json:"status"`
	ChangedFiles []string `json:"changed_files"`
	Patch        string   `json:"patch"`
}
```
Add fields to `StreamEvent`:
```go
	Review *ReviewData `json:"review,omitempty"`
	Diff   *DiffData   `json:"diff,omitempty"`
```

In `internal/operate/runtime.go` `callTool`, AFTER `Tools.Execute` succeeds and the result entry is appended + StreamToolEnd emitted (i.e. in the success path, only when `runErr == nil`), emit the structured event:
```go
	switch call.Name {
	case "review_worker":
		emit(StreamEvent{Kind: StreamReview, Review: reviewDataFromResult(result.Data)})
	case "diff_worker":
		emit(StreamEvent{Kind: StreamDiff, Diff: diffDataFromResult(result.Data)})
	}
```
Add the parsers (in runtime.go):
```go
func reviewDataFromResult(data map[string]any) *ReviewData {
	rd := &ReviewData{}
	if s, ok := data["summary"].(string); ok {
		rd.Summary = s
	}
	if raw, ok := data["findings"].([]map[string]any); ok {
		for _, f := range raw {
			rd.Findings = append(rd.Findings, findingFromMap(f))
		}
	} else if rawAny, ok := data["findings"].([]any); ok {
		for _, item := range rawAny {
			if f, ok := item.(map[string]any); ok {
				rd.Findings = append(rd.Findings, findingFromMap(f))
			}
		}
	}
	return rd
}

func findingFromMap(f map[string]any) Finding {
	out := Finding{}
	if v, ok := f["severity"].(string); ok {
		out.Severity = v
	}
	if v, ok := f["file"].(string); ok {
		out.File = v
	}
	switch n := f["line"].(type) {
	case int:
		out.Line = n
	case float64:
		out.Line = int(n)
	}
	if v, ok := f["title"].(string); ok {
		out.Title = v
	}
	if v, ok := f["body"].(string); ok {
		out.Body = v
	}
	if v, ok := f["action"].(string); ok {
		out.Action = v
	}
	return out
}

func diffDataFromResult(data map[string]any) *DiffData {
	dd := &DiffData{}
	if s, ok := data["status"].(string); ok {
		dd.Status = s
	}
	if s, ok := data["diff"].(string); ok {
		dd.Patch = s
	}
	switch cf := data["changed_files"].(type) {
	case []string:
		dd.ChangedFiles = cf
	case []any:
		for _, item := range cf {
			if s, ok := item.(string); ok {
				dd.ChangedFiles = append(dd.ChangedFiles, s)
			}
		}
	}
	return dd
}
```

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/operate/ -run TestRunTurnEmitsReviewAndDiff -v`, then full `go test ./internal/operate/`.

- [ ] **Step 5: Build, vet, commit.**
`go build ./... && go vet ./internal/operate/ && go test ./internal/operate/ && gofmt -l internal/operate/`
```bash
git add internal/operate/runtime_stream.go internal/operate/runtime.go internal/operate/review_events_test.go
git commit -m "feat(operate): emit structured StreamReview/StreamDiff events"
```

---

## Task 4.2: Review overlay (findings inbox + read-only diff) in the cockpit

**Files:** Modify `internal/tui/operate.go`, `internal/tui/operate_view.go`; Test `internal/tui/operate_test.go`.

- [ ] **Step 1: Failing test** — append to `internal/tui/operate_test.go`:

```go
func TestOperateReviewOverlay(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	m.applyStream(operate.StreamEvent{Kind: operate.StreamReview, Review: &operate.ReviewData{
		Summary: "1 finding",
		Findings: []operate.Finding{
			{Severity: "high", File: "feeds.go", Line: 42, Title: "no timeout on HTTP", Body: "wrap in context"},
		},
	}})
	if len(m.findings) != 1 {
		t.Fatalf("findings not stored: %d", len(m.findings))
	}
	// open the review overlay
	m.showReview = true
	out := m.render()
	if !strings.Contains(out, "feeds.go") || !strings.Contains(out, "no timeout") {
		t.Fatalf("review overlay did not render the finding:\n%s", out)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (no `findings`/`showReview`). `go test ./internal/tui/ -run TestOperateReviewOverlay -v`

- [ ] **Step 3: Implement.**

In `internal/tui/operate.go`:
- Add fields to `operateModel`: `findings []operate.Finding`, `diff *operate.DiffData`, `reviewSummary string`, `showReview bool`, `reviewIndex int`, `findingState map[int]string` (index→"accepted"/"dismissed").
- In `applyStream`, add cases:
```go
	case operate.StreamReview:
		if ev.Review != nil {
			m.findings = ev.Review.Findings
			m.reviewSummary = ev.Review.Summary
			m.reviewIndex = 0
			m.findingState = map[int]string{}
			if len(m.findings) > 0 {
				m.showReview = true
			}
		}
	case operate.StreamDiff:
		if ev.Diff != nil {
			m.diff = ev.Diff
		}
```
- In `handleKey`, add (when not in an approval and not typing into composer specially):
  - `"ctrl+r"`: toggle `m.showReview` if there are findings (or a diff).
  - When `m.showReview` is true, intercept navigation BEFORE composer handling:
    - `"up"`/`"down"`: move `reviewIndex` within `len(m.findings)`.
    - `"f"`: fix the selected finding → set `m.showReview=false` and `return m.submit(fixPromptFor(m.findings[m.reviewIndex]))`.
    - `"a"`: `m.findingState[m.reviewIndex] = "accepted"`.
    - `"x"`: `m.findingState[m.reviewIndex] = "dismissed"`.
    - `"esc"`/`"q"`: `m.showReview = false`.
  Implement this as a `handleReviewKey(keyStr)` method called at the top of `handleKey` when `m.showReview` (after the pendingApproval interception). Add helper:
```go
func fixPromptFor(f operate.Finding) string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return fmt.Sprintf("fix this finding: %s (%s)", f.Title, loc)
}
```

In `internal/tui/operate_view.go`:
- In `render()`, when `m.showReview`, render the review overlay INSTEAD of the normal transcript region (like the help overlay): `if m.showReview { return m.renderReview() }` near where `m.showHelp` is handled.
- Add `renderReview() string`:
```go
func (m *operateModel) renderReview() string {
	title := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	sel := lipgloss.NewStyle().Foreground(lipgloss.Color(blackHex)).Background(lipgloss.Color(greenHex))
	val := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))

	var lines []string
	lines = append(lines, title.Render("Review — "+m.reviewSummary))
	lines = append(lines, "")
	for i, f := range m.findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		marker := severityGlyph(f.Severity)
		row := fmt.Sprintf("%s %-8s %s  %s", marker, f.Severity, loc, f.Title)
		if st := m.findingState[i]; st != "" {
			row += "  [" + st + "]"
		}
		if i == m.reviewIndex {
			lines = append(lines, sel.Render(" "+row+" "))
		} else {
			lines = append(lines, val.Render("  "+row))
		}
	}
	if len(m.findings) == 0 {
		lines = append(lines, muted.Render("No findings."))
	}
	// detail + diff for the selected finding
	if m.reviewIndex < len(m.findings) {
		f := m.findings[m.reviewIndex]
		lines = append(lines, "", title.Render("Detail"), val.Render(wrapText(f.Body, m.width-4)))
		if f.Action != "" {
			lines = append(lines, muted.Render("fix: ")+val.Render(wrapText(f.Action, m.width-8)))
		}
	}
	if m.diff != nil && strings.TrimSpace(m.diff.Patch) != "" {
		lines = append(lines, "", title.Render("Diff"))
		for _, ln := range renderDiffLines(m.diff.Patch, m.width-2) {
			lines = append(lines, ln)
		}
	}
	lines = append(lines, "", muted.Render("↑↓ select  f fix(agent)  a accept  x dismiss  esc/q close"))
	box := lipgloss.NewStyle().Padding(0, 1).Width(max(m.width-2, 20))
	return box.Render(strings.Join(lines, "\n"))
}

func severityGlyph(sev string) string {
	switch strings.ToLower(sev) {
	case "critical":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Bold(true).Render("■")
	case "high":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Render("▲")
	case "medium":
		return lipgloss.NewStyle().Foreground(lipgloss.Color(yellowHex)).Render("●")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex)).Render("○")
	}
}

// renderDiffLines colors +/- lines of a unified diff (read-only), capped.
func renderDiffLines(patch string, width int) []string {
	add := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	del := lipgloss.NewStyle().Foreground(lipgloss.Color(redHex))
	hdr := lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	var out []string
	n := 0
	for _, ln := range strings.Split(patch, "\n") {
		if n >= 200 {
			out = append(out, muted.Render("… diff truncated"))
			break
		}
		switch {
		case strings.HasPrefix(ln, "+"):
			out = append(out, add.Render(ln))
		case strings.HasPrefix(ln, "-"):
			out = append(out, del.Render(ln))
		case strings.HasPrefix(ln, "@@"):
			out = append(out, hdr.Render(ln))
		default:
			out = append(out, muted.Render(ln))
		}
		n++
	}
	return out
}
```
- Add a hint to the status bar or footer that `Ctrl+R` opens review when findings exist (optional; the test only needs the overlay render).

(`max`, `wrapText`, `kv`, color constants exist in the tui package. `fmt`, `strings`, `sort` already imported where needed; add `fmt` to operate.go if missing.)

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ -run 'TestOperate' -v`

- [ ] **Step 5: Build whole repo, vet, full test, commit.**
`go build ./... && go vet ./... && go test ./... && gofmt -l internal/tui/ internal/operate/`
```bash
git add internal/tui/operate.go internal/tui/operate_view.go internal/tui/operate_test.go
git commit -m "feat(tui): review overlay — findings inbox + read-only diff viewer"
```

---

## Final verification
- [ ] `go build ./... && go vet ./... && go test ./...` — all green.

## Self-Review
- AC-R1 (diff renders with +/- and @@) → renderDiffLines. AC-R2 (inbox open/fix/accept/dismiss) → handleReviewKey (f→agent fix, a/x→state). AC-F1/F2 (gate failures surface; fix re-audits) → fix routes to fix_worker via submit, which re-audits (existing FixWorker→audit path). Full manual editor (Slice 4.5) and finding-state→deploy-block (spec §6 prod rule) are deferred to a follow-up; this slice delivers the read-only inbox + diff + agent-fix loop.
- Type consistency: `operate.ReviewData`/`DiffData`/`Finding`, `StreamReview`/`StreamDiff` from 4.1 consumed by the TUI in 4.2. `m.findings []operate.Finding`, `m.diff *operate.DiffData`.
- Placeholders: none.
