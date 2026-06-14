# Agentic Harness — UX SOTA Finish Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps.

**Goal:** Close the remaining UX gaps so the cockpit feels SOTA like Pi / Claude Code: collapsible tool cards (noise reduction), `!cmd` shell-into-context, an in-app manual file editor (hand-audit/edit), and `/new`+`/clear` session control. (Inline non-alt-screen differential rendering stays deferred per the design spec — it risks regressing the working TUI.)

**Architecture:** All changes live in `internal/tui/operate.go` + `operate_view.go` (+ one read-only/gated file IO helper in `internal/operate` for the editor). Each item is independent and testable on the Bubble Tea model directly.

**Tech Stack:** Go 1.25, bubbles v2 (textarea/viewport), lipgloss. Tests drive the model synchronously (no goroutines/network).

---

## Task U1: Collapsible tool cards (Ctrl+O)

Completed tool cards collapse to a one-line summary by default; `Ctrl+O` toggles expand/collapse for all tool blocks. Running cards always render expanded.

**Files:** `internal/tui/operate.go`, `internal/tui/operate_view.go`, test `internal/tui/operate_test.go`.

- [ ] **Step 1: Failing test** (append to operate_test.go):
```go
func TestToolCardsCollapseByDefault(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	start := &operate.TranscriptEntry{Kind: operate.TranscriptToolCall, ToolName: "read_ouvrier_api", Input: map[string]any{}}
	m.applyStream(operate.StreamEvent{Kind: operate.StreamToolStart, Entry: start})
	end := &operate.TranscriptEntry{Kind: operate.TranscriptToolResult, ToolName: "read_ouvrier_api", Output: map[string]any{"summary": "loaded API ref"}}
	m.applyStream(operate.StreamEvent{Kind: operate.StreamToolEnd, Entry: end})

	// the completed tool block must be collapsed
	var tool *opBlock
	for i := range m.blocks {
		if m.blocks[i].kind == blockTool {
			tool = &m.blocks[i]
		}
	}
	if tool == nil || !tool.collapsed {
		t.Fatalf("completed tool card should default to collapsed: %+v", tool)
	}
	out := m.render()
	if !strings.Contains(out, "read_ouvrier_api") {
		t.Fatalf("collapsed card should still show the tool name:\n%s", out)
	}
	// Ctrl+O expands
	m.handleKey(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	for i := range m.blocks {
		if m.blocks[i].kind == blockTool && m.blocks[i].collapsed {
			t.Fatal("ctrl+o should expand all tool cards")
		}
	}
}
```
(If `tea.KeyPressMsg{Code:'o', Mod: tea.ModCtrl}` does not stringify to "ctrl+o" in this bubbletea version, the implementer should call `m.handleKey` with whatever KeyPressMsg yields `.String()=="ctrl+o"`; verify by checking how existing tests build keys.)

- [ ] **Step 2: Run, expect FAIL.** `go test ./internal/tui/ -run TestToolCardsCollapse -v`

- [ ] **Step 3: Implement.**
- `opBlock` gains `collapsed bool`.
- In `applyStream` `StreamToolEnd`: after marking the card finished, set `m.blocks[m.runningToolIdx].collapsed = true`.
- Add `Ctrl+O` to `handleKey`: toggle a model field `m.toolsExpanded bool`; when toggled, set every `blockTool`'s `collapsed = !m.toolsExpanded` (expanded => collapsed=false). Then `m.refreshViewport()`.
- In `renderBlockTool`: if `b.collapsed && !b.running`, render a single line: `⚙ <name> <✓/✗>  → <summary>` (reuse the badge + summary logic, one line, with a leading `▸`). Otherwise render the full multi-line card with a leading `▾` on the header.

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ -run 'TestOperate|TestToolCards' -v`

- [ ] **Step 5: Build + commit.** `go build ./... && go test ./internal/tui/ && gofmt -l internal/tui/`
```bash
git add internal/tui/operate.go internal/tui/operate_view.go internal/tui/operate_test.go
git commit -m "feat(tui): collapsible tool cards (Ctrl+O), collapsed by default"
```

---

## Task U2: `!cmd` shell-into-context escape hatch

Typing `!<command>` runs a shell command in the worker dir and adds its output as a transcript notice block (the Pi/Claude-Code escape hatch). `!!<command>` runs it but does not add output (silent). It does NOT go to the model; it's an operator utility.

**Files:** `internal/tui/operate.go`, test `internal/tui/operate_test.go`.

- [ ] **Step 1: Failing test** (append):
```go
func TestBangCommandRunsShell(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	m.submit("!echo hello-ouvrier")
	out := m.render()
	if !strings.Contains(out, "hello-ouvrier") {
		t.Fatalf("!cmd output not shown in transcript:\n%s", out)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** In `submit` (operate.go), at the very top after trimming, intercept `!`:
```go
	if strings.HasPrefix(text, "!") {
		return m.runShell(text)
	}
```
Add:
```go
func (m *operateModel) runShell(text string) (tea.Model, tea.Cmd) {
	silent := strings.HasPrefix(text, "!!")
	cmdline := strings.TrimSpace(strings.TrimLeft(text, "!"))
	if cmdline == "" {
		return m, nil
	}
	m.blocks = append(m.blocks, opBlock{kind: blockUser, text: "!" + cmdline})
	dir := m.workspace.Dir
	if dir == "" {
		dir = m.opts.Dir
	}
	c := exec.CommandContext(m.ctx, "sh", "-c", cmdline)
	c.Dir = dir
	outBytes, err := c.CombinedOutput()
	out := strings.TrimRight(string(outBytes), "\n")
	if err != nil && out == "" {
		out = err.Error()
	}
	if !silent {
		if out == "" {
			out = "(no output)"
		}
		m.blocks = append(m.blocks, opBlock{kind: blockNotice, text: out})
	}
	m.refreshViewport()
	return m, nil
}
```
Add `"os/exec"` to operate.go imports.

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ -run 'TestBangCommand|TestOperate' -v`

- [ ] **Step 5: Build + commit.**
```bash
git add internal/tui/operate.go internal/tui/operate_test.go
git commit -m "feat(tui): !cmd / !!cmd shell-into-context escape hatch"
```

---

## Task U3: Manual file editor overlay (hand-edit + re-audit)

`/edit <path>` (or `Ctrl+E` with a worker selected) opens a full-screen editor on a worker file; `Ctrl+S` saves and queues a re-audit; `Esc` cancels. Read/write are sandboxed to the worker dir.

**Files:** `internal/operate/workspace.go` (add safe read/write helpers if absent) or reuse existing; `internal/tui/operate.go`, `internal/tui/operate_view.go`, tests.

- [ ] **Step 1: Failing test** (append to operate_test.go):
```go
func TestManualEditorOpenSaveReaudit(t *testing.T) {
	dir := t.TempDir()
	wdir := filepath.Join(dir, "demo")
	writeOperateWorker(t, wdir, "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: wdir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	if _, err := m.openEditor("main.go"); err != nil {
		t.Fatalf("open editor: %v", err)
	}
	if !m.showEditor || m.editorPath != "main.go" {
		t.Fatalf("editor not open: show=%v path=%q", m.showEditor, m.editorPath)
	}
	out := m.render()
	if !strings.Contains(out, "main.go") {
		t.Fatalf("editor overlay should show the path:\n%s", out)
	}
	// set new content and save
	m.editor.SetValue("package main\n\nfunc main() { /* edited */ }\n")
	if err := m.saveEditor(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(wdir, "main.go"))
	if !strings.Contains(string(data), "edited") {
		t.Fatalf("file not saved with new content: %s", data)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.**
- Add a sandboxed read/write helper. Reuse the path guard from `internal/operate/tools.go` (`requireWorkspace` + the unsafe-path check in `toolReadWorkerFile`). Add to `internal/operate` two exported helpers if not present:
```go
// ReadWorkerFile reads a worker-relative file (sandboxed to ws.Dir).
func ReadWorkerFile(ws Workspace, rel string) (string, error)
// WriteWorkerFile writes a worker-relative file (sandboxed to ws.Dir).
func WriteWorkerFile(ws Workspace, rel string, content string) error
```
Implement both with the same path safety as `toolReadWorkerFile` (reject abs paths, `..`, `.git`). WriteWorkerFile uses `writeAtomic` with 0o644.
- In `internal/tui/operate.go`: add fields `showEditor bool`, `editorPath string`, `editor textarea.Model`, `editorErr string`. Add:
```go
func (m *operateModel) openEditor(path string) (tea.Model, tea.Cmd) {
	ws := m.workspace
	content, err := operate.ReadWorkerFile(ws, path)
	if err != nil {
		m.blocks = append(m.blocks, opBlock{kind: blockError, text: "open " + path + ": " + err.Error()})
		m.refreshViewport()
		return m, nil
	}
	ta := textarea.New()
	ta.SetValue(content)
	ta.ShowLineNumbers = true
	ta.SetWidth(max(m.width-2, 40))
	ta.SetHeight(max(m.height-4, 6))
	ta.Focus()
	m.editor = ta
	m.editorPath = path
	m.showEditor = true
	return m, nil
}

func (m *operateModel) saveEditor() error {
	if err := operate.WriteWorkerFile(m.workspace, m.editorPath, m.editor.Value()); err != nil {
		m.editorErr = err.Error()
		return err
	}
	m.showEditor = false
	m.blocks = append(m.blocks, opBlock{kind: blockNotice, text: "saved " + m.editorPath + " — re-auditing"})
	m.refreshViewport()
	return nil
}
```
- Wire keys: in `handleKey`, when `m.showEditor`, route to a `handleEditorKey`: `Ctrl+S` → `saveEditor()` then `return m.submit("audit the worker")` (re-audit via the agent/planner); `Esc` → `m.showEditor=false`; otherwise forward the key to `m.editor` (`m.editor, cmd = m.editor.Update(msg)`). Add `/edit <path>` handling: in `submit`, if the text is `/edit <path>` open the editor (parse path; default main.go). Also add `Ctrl+E` to open `main.go` when a worker is selected.
- In `operate_view.go` `render()`: `if m.showEditor { return m.renderEditor() }` (near showHelp/showReview). `renderEditor` shows a header (`✎ <path>  Ctrl+S save & re-audit · Esc cancel`), the `m.editor.View()`, and any `editorErr`.

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ ./internal/operate/ -run 'TestManualEditor|TestOperate|TestWorkerFile' -v`

- [ ] **Step 5: Build, vet, commit.**
```bash
git add internal/operate/ internal/tui/operate.go internal/tui/operate_view.go internal/tui/operate_test.go
git commit -m "feat(tui): manual file editor overlay (Ctrl+E / /edit), save -> re-audit"
```

---

## Task U4: `/new` and `/clear` session control + slash list update

`/clear` starts a fresh session in the same workspace (clears the transcript view); `/new` is already a worker-scaffold accelerator — keep it but add `/clear`. Add `/edit` and `!cmd` to the slash help and the `?` help overlay.

**Files:** `internal/tui/operate.go` (slash list + handling), `internal/tui/operate_view.go` (help), test.

- [ ] **Step 1: Failing test** (append):
```go
func TestSlashClearStartsFreshTranscript(t *testing.T) {
	dir := t.TempDir()
	wdir := filepath.Join(dir, "demo")
	writeOperateWorker(t, wdir, "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: wdir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.blocks = append(m.blocks, opBlock{kind: blockUser, text: "old turn"})
	m.submit("/clear")
	for _, b := range m.blocks {
		if b.kind == blockUser && strings.Contains(b.text, "old turn") {
			t.Fatal("/clear should drop the previous transcript blocks")
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** In `submit`, intercept `/clear` before the normal prompt path:
```go
	if strings.TrimSpace(text) == "/clear" {
		m.blocks = nil
		m.findings = nil
		m.diff = nil
		m.refreshViewport()
		return m, nil
	}
```
Add `{"/clear", "/clear", "Clear the transcript view"}` and `{"/edit", "/edit main.go", "Open a worker file in the manual editor"}` to `operateSlashCommands`. Route `/edit <path>` in submit to `openEditor`. Update `renderHints`/help to mention `!cmd` and `Ctrl+E`/`Ctrl+O`.

- [ ] **Step 4: Run, expect PASS.** `go test ./internal/tui/ -run 'TestSlash|TestOperate' -v`

- [ ] **Step 5: Build, full test, commit.**
`go build ./... && go vet ./... && go test ./... && gofmt -l internal/tui/`
```bash
git add internal/tui/operate.go internal/tui/operate_view.go internal/tui/operate_test.go
git commit -m "feat(tui): /clear session, /edit command, updated help/hints"
```

---

## Final verification
- [ ] `go build ./... && go vet ./... && go test ./...` — all green.
- [ ] Visual: `OVR_DEMO=1 go test ./internal/tui/ -run RenderDemo -v` — confirm collapsed cards + clean layout.

## Self-Review
- SOTA parity items added: collapsible tool cards (U1), `!cmd` escape hatch (U2), manual editor (U3), `/clear`+`/edit` (U4). Model picker stays as Ctrl+P cycle (cross-provider switch needs cli-layer factory — out of scope). Inline rendering deferred (risk).
- Type consistency: `opBlock.collapsed` (U1) used in renderBlockTool; `operate.ReadWorkerFile/WriteWorkerFile` (U3) consumed by editor; editor fields on operateModel.
- Placeholders: none.
