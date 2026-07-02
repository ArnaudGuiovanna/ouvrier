package ide

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func writeIDEWorker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/worker\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: worker\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ouvrier.worker.json"), []byte(`{"name":"worker","main":"main.go"}`), 0o644)
	return dir
}

func makeIDEWorkspace(t *testing.T, dir string) operate.Workspace {
	t.Helper()
	return operate.Workspace{
		Dir:      dir,
		Name:     "worker",
		MainPath: filepath.Join(dir, "main.go"),
	}
}

func TestIDEOpensAndRenders(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)

	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	// Send a window size message so the model becomes ready.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ideModel)

	view := m.View()
	out := view.Content
	if out == "" {
		t.Fatal("View().Content is empty after WindowSizeMsg")
	}
	if !view.AltScreen {
		t.Fatal("View().AltScreen should be true")
	}
	if view.BackgroundColor == nil {
		t.Fatal("View().BackgroundColor should be set")
	}
	if view.WindowTitle != "ouvrier ide" {
		t.Fatalf("View().WindowTitle = %q, want %q", view.WindowTitle, "ouvrier ide")
	}
}

func TestIDESaveWritesFile(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)

	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	// Initialize to load file content.
	cmds := m.Init()
	_ = cmds // cmds run asynchronously in real usage

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ideModel)

	// Set editor content and simulate Ctrl+S.
	m.editor.SetValue("package main\n\nfunc main() { /* saved by test */ }\n")
	m.dirty = true

	result, _ := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = result.(*ideModel)

	// The file should have been written.
	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !strings.Contains(string(data), "saved by test") {
		t.Fatalf("file content not saved; got: %s", data)
	}
	if m.dirty {
		t.Fatal("dirty flag should be cleared after ctrl+s")
	}
}

func TestIDEDiagnosticsToProblems(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)

	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// Inject a diagnostic message.
	mainURI := lsp.URI(filepath.Join(dir, "main.go"))
	diagParams := lsp.PublishDiagnosticsParams{
		URI: mainURI,
		Diagnostics: []lsp.Diagnostic{
			{
				Range:    lsp.Range{Start: lsp.Position{Line: 2, Character: 0}},
				Severity: 1,
				Message:  "undefined: Foo",
				Source:   "compiler",
			},
		},
	}
	updated, _ := m.Update(diagMsg{params: diagParams})
	m = updated.(*ideModel)

	if len(m.problems) != 1 {
		t.Fatalf("expected 1 problem after diagMsg, got %d", len(m.problems))
	}
	p := m.problems[0]
	if p.Source != "lsp" {
		t.Errorf("problem source = %q, want lsp", p.Source)
	}
	if p.Severity != 1 {
		t.Errorf("problem severity = %d, want 1", p.Severity)
	}
	if !strings.Contains(p.Message, "Foo") {
		t.Errorf("problem message = %q, should contain Foo", p.Message)
	}
}

func TestIDEFocusCyclesWithTab(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)
	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// Initial focus is editor.
	if m.focus != regionEditor {
		t.Fatalf("initial focus = %d, want regionEditor (%d)", m.focus, regionEditor)
	}

	// Tab -> problems.
	result, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = result.(*ideModel)
	if m.focus != regionProblems {
		t.Fatalf("after tab: focus = %d, want regionProblems (%d)", m.focus, regionProblems)
	}

	// Tab -> tree.
	result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = result.(*ideModel)
	if m.focus != regionTree {
		t.Fatalf("after 2nd tab: focus = %d, want regionTree (%d)", m.focus, regionTree)
	}

	// Tab -> editor again.
	result, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m = result.(*ideModel)
	if m.focus != regionEditor {
		t.Fatalf("after 3rd tab: focus = %d, want regionEditor (%d)", m.focus, regionEditor)
	}
}

func TestIDELspStatusNoGopls(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)
	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	if !strings.Contains(m.lspStatus, "gopls not found") {
		t.Fatalf("lspStatus = %q, want 'gopls not found' when GoplsPath is empty", m.lspStatus)
	}
}

func TestEmbeddedQuitReturnsExitMsg(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)

	// --- embedded: ctrl+q must yield ExitMsg, not tea.Quit ---
	me := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: "", Embedded: true})
	me.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	_, cmd := me.Update(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("embedded ctrl+q: expected a cmd, got nil")
	}
	msg := cmd()
	if _, ok := msg.(ExitMsg); !ok {
		t.Fatalf("embedded ctrl+q: expected ExitMsg, got %T (%v)", msg, msg)
	}

	// --- non-embedded: ctrl+q must NOT yield ExitMsg ---
	mn := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: "", Embedded: false})
	mn.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	_, cmd2 := mn.Update(tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl})
	// The non-embedded path uses tea.Sequence which returns a batchMsg
	// containing the shutdown + tea.Quit funcs. Run the batch cmd.
	if cmd2 != nil {
		msg2 := cmd2()
		if _, ok := msg2.(ExitMsg); ok {
			t.Fatal("non-embedded ctrl+q: must NOT return ExitMsg")
		}
	}
}

func TestIDEAuditMsgUpdatesStatus(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)
	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// Inject a passing audit.
	report := operate.AuditReport{
		Passed: true,
		Results: []operate.GateResult{
			{Name: "gofmt", Status: operate.GatePass},
		},
	}
	result, _ := m.Update(auditMsg{report: report})
	m = result.(*ideModel)
	if m.statusKind != "ok" {
		t.Fatalf("statusKind = %q after passing audit, want ok", m.statusKind)
	}
	if !m.auditPassed {
		t.Fatal("auditPassed should be true after passing audit")
	}

	// Inject a failing audit.
	report2 := operate.AuditReport{
		Passed: false,
		Results: []operate.GateResult{
			{Name: "go test ./...", Status: operate.GateFail, Error: "tests failed"},
		},
	}
	result, _ = m.Update(auditMsg{report: report2})
	m = result.(*ideModel)
	if m.statusKind != "fail" {
		t.Fatalf("statusKind = %q after failing audit, want fail", m.statusKind)
	}
	if m.auditPassed {
		t.Fatal("auditPassed should be false after failing audit")
	}
	if len(m.auditProbs) == 0 {
		t.Fatal("auditProbs should have entries after failing audit")
	}
}
