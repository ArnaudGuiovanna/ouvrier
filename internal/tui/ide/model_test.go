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

// newGovernedIDE returns an ideModel wired to a real GovernedExecutor and
// session, the way the cockpit and `ouvrier ide` open it.
func newGovernedIDE(t *testing.T, dir string, ws operate.Workspace) (*ideModel, *operate.Session) {
	t.Helper()
	rt, err := operate.NewAgentRuntime(operate.RuntimeOptions{Dir: dir, Driver: operate.ManualDriver{}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	started, err := rt.Start(context.Background(), operate.RuntimeStartRequest{Dir: dir})
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	m := newIDEModel(context.Background(), IDEOptions{
		Workspace: ws,
		GoplsPath: "",
		Executor:  rt.Executor(),
		Session:   started.Session,
	})
	return m, started.Session
}

func TestIDESaveWritesFile(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)

	m, session := newGovernedIDE(t, dir, ws)
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

	// The governed save must leave a transcript tool_call/tool_result pair and
	// one tool-call audit record.
	transcript, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"tool_name":"write_worker_file"`) {
		t.Fatalf("transcript missing governed write_worker_file record:\n%s", transcript)
	}
	audit, err := os.ReadFile(session.ToolCallsPath)
	if err != nil {
		t.Fatalf("read tool-calls audit: %v", err)
	}
	if !strings.Contains(string(audit), `"tool":"write_worker_file"`) {
		t.Fatalf("tool-calls audit missing write_worker_file record:\n%s", audit)
	}
}

// Without a governed executor the IDE save fails closed: no write, no silent
// bypass of transcript/audit.
func TestIDESaveWithoutExecutorFailsClosed(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)

	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.editor.SetValue("package main\n\nfunc main() { /* must not land */ }\n")
	m.dirty = true

	result, _ := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = result.(*ideModel)

	data, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(data), "must not land") {
		t.Fatal("ungoverned IDE save must not write the file")
	}
	if !m.dirty {
		t.Fatal("dirty flag must survive a refused save")
	}
	if m.statusKind != "fail" || !strings.Contains(m.status, "save failed") {
		t.Fatalf("status = %q/%q, want failed save status", m.statusKind, m.status)
	}
}

// fakeExecutor records governed calls for routing assertions.
type fakeExecutor struct {
	calls  []operate.GovernedCall
	result operate.ToolResult
	err    error
}

func (f *fakeExecutor) Execute(_ context.Context, call operate.GovernedCall) (operate.ToolResult, error) {
	f.calls = append(f.calls, call)
	return f.result, f.err
}

// ctrl+s audits and ctrl+b builds through the GovernedExecutor, not through
// direct AuditRunner/BuildCoordinator calls.
func TestIDEAuditAndBuildRouteThroughExecutor(t *testing.T) {
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)
	fake := &fakeExecutor{result: operate.ToolResult{
		Summary: "audit passed with 1 gate(s)",
		Data: map[string]any{
			"passed":      true,
			"gates":       []map[string]any{{"name": "gofmt", "status": operate.GatePass}},
			"binary_path": "/tmp/worker-bin",
		},
	}}
	m := newIDEModel(context.Background(), IDEOptions{
		Workspace: ws,
		GoplsPath: "",
		Executor:  fake,
		Session:   &operate.Session{ID: "s1"},
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	msg := m.runAudit()()
	am, ok := msg.(auditMsg)
	if !ok {
		t.Fatalf("runAudit returned %T, want auditMsg", msg)
	}
	if am.err != nil || !am.report.Passed || len(am.report.Results) != 1 {
		t.Fatalf("audit report not reconstructed: %+v err=%v", am.report, am.err)
	}
	msg = m.runBuild()()
	bm, ok := msg.(buildMsg)
	if !ok {
		t.Fatalf("runBuild returned %T, want buildMsg", msg)
	}
	if bm.err != nil || bm.artifact.BinaryPath != "/tmp/worker-bin" {
		t.Fatalf("build artifact not reconstructed: %+v err=%v", bm.artifact, bm.err)
	}

	if len(fake.calls) != 2 {
		t.Fatalf("executor calls = %d, want 2", len(fake.calls))
	}
	if fake.calls[0].Tool != "audit_worker" || fake.calls[1].Tool != "build_worker" {
		t.Fatalf("executor tools = %q, %q; want audit_worker, build_worker", fake.calls[0].Tool, fake.calls[1].Tool)
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
