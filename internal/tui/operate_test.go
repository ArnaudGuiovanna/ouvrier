package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/tui/ide"
)

func TestOperateModelSelectsWorkerCandidate(t *testing.T) {
	parent := t.TempDir()
	writeOperateWorker(t, filepath.Join(parent, "alpha"), "alpha")
	writeOperateWorker(t, filepath.Join(parent, "beta"), "beta")

	model := newOperateModel(context.Background(), OperateOptions{Dir: parent, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	if model.mode != "select" || len(model.candidates) != 2 || model.session == nil {
		t.Fatalf("initial model mode=%q candidates=%d session=%v", model.mode, len(model.candidates), model.session)
	}

	updated, _ := model.Update(tea.KeyPressMsg{Code: '1', Text: "1"})
	selected := updated.(*operateModel)
	if selected.session == nil {
		t.Fatal("session = nil after selecting candidate")
	}
	if selected.workspace.Name != "alpha" {
		t.Fatalf("workspace name = %q, want alpha", selected.workspace.Name)
	}
	if selected.mode != "operate" {
		t.Fatalf("mode = %q, want operate", selected.mode)
	}
}

func TestOperateModelStreamsTurnIntoBlocks(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, dir, "ticket-triage")

	model := newOperateModel(context.Background(), OperateOptions{Dir: dir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	model.submit("/workers")
	if !model.running {
		t.Fatal("model should be running after submit")
	}

	// Drain the live stream the way the Bubble Tea loop would.
	for ev := range model.events {
		model.handleStream(opStreamMsg{ev: ev, ok: true})
	}
	model.handleStream(opStreamMsg{ok: false})

	if model.running {
		t.Fatal("model still running after stream closed")
	}

	var sawUser, sawTool bool
	for _, b := range model.blocks {
		switch b.kind {
		case blockUser:
			if strings.Contains(b.text, "/workers") {
				sawUser = true
			}
		case blockTool:
			if b.toolName == "list_workers" && !b.running {
				sawTool = true
			}
		}
	}
	if !sawUser {
		t.Fatalf("transcript missing user block; blocks=%+v", model.blocks)
	}
	if !sawTool {
		t.Fatalf("transcript missing completed list_workers tool card; blocks=%+v", model.blocks)
	}

	out := model.render()
	if !strings.Contains(out, "list_workers") {
		t.Fatalf("rendered cockpit missing tool card name:\n%s", out)
	}
	if !strings.Contains(out, "ready") && !strings.Contains(out, "working") {
		t.Fatalf("rendered cockpit missing status bar:\n%s", out)
	}
}

func TestOperateApprovalCardFlow(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

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

func TestApprovalKeyApproveAndDeny(t *testing.T) {
	// non-prod approve via enter
	dec := make(chan operate.ApprovalDecision, 1)
	m := &operateModel{decisions: dec}
	m.pendingApproval = &operate.ApprovalRequest{ID: "a1", Tool: "build_worker"}
	m.handleApprovalKey("enter")
	d := <-dec
	if !d.Approved || d.ID != "a1" {
		t.Fatalf("enter should approve a1: %+v", d)
	}
	if m.pendingApproval != nil {
		t.Fatal("pendingApproval not cleared after approve")
	}

	// non-prod deny via esc
	m.decisions = dec
	m.pendingApproval = &operate.ApprovalRequest{ID: "a2", Tool: "build_worker"}
	m.handleApprovalKey("esc")
	d = <-dec
	if d.Approved || d.ID != "a2" {
		t.Fatalf("esc should deny a2: %+v", d)
	}
	if m.pendingApproval != nil {
		t.Fatal("pendingApproval not cleared after deny")
	}
}

func TestApprovalKeyProdTypedConfirm(t *testing.T) {
	dec := make(chan operate.ApprovalDecision, 1)
	m := &operateModel{decisions: dec}
	m.pendingApproval = &operate.ApprovalRequest{ID: "p1", Tool: "transfer_worker", Prod: true, Details: map[string]any{"worker": "demo"}}

	// enter before typing the name must NOT approve and must NOT clear the card
	m.handleApprovalKey("enter")
	select {
	case d := <-dec:
		t.Fatalf("prod approved without typing the name: %+v", d)
	default:
	}
	if m.pendingApproval == nil {
		t.Fatal("prod card cleared prematurely")
	}

	// type the worker name, then enter approves
	for _, c := range "demo" {
		m.handleApprovalKey(string(c))
	}
	m.handleApprovalKey("enter")
	d := <-dec
	if !d.Approved || d.ID != "p1" {
		t.Fatalf("prod should approve after typing name: %+v", d)
	}
}

func TestOperateAuthNoticeWhenSignedOut(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}, AuthState: "unauthed"}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	out := m.render()
	if !strings.Contains(out, "sign in") && !strings.Contains(out, "/login") {
		t.Fatalf("expected sign-in hint in status/footer:\n%s", out)
	}
}

func TestOperateAuthShowsAccountWhenSignedIn(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}, AuthState: "authed", AuthAccount: "Logged in using ChatGPT"}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	out := m.render()
	if !strings.Contains(out, "ChatGPT") && !strings.Contains(out, "auth") {
		t.Fatalf("expected signed-in account/auth in status bar:\n%s", out)
	}
}

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
	if !m.showReview {
		t.Fatal("applyStream with findings did not auto-open the review overlay")
	}
	out := m.render()
	if !strings.Contains(out, "feeds.go") || !strings.Contains(out, "no timeout") {
		t.Fatalf("review overlay did not render the finding:\n%s", out)
	}
}

func TestToolCardsCollapseByDefault(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	start := &operate.TranscriptEntry{Kind: operate.TranscriptToolCall, ToolName: "read_ouvrier_api", Input: map[string]any{}}
	m.applyStream(operate.StreamEvent{Kind: operate.StreamToolStart, Entry: start})
	end := &operate.TranscriptEntry{Kind: operate.TranscriptToolResult, ToolName: "read_ouvrier_api", Output: map[string]any{"summary": "loaded API ref"}}
	m.applyStream(operate.StreamEvent{Kind: operate.StreamToolEnd, Entry: end})
	m.refreshViewport()

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
	m.handleKey(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	for i := range m.blocks {
		if m.blocks[i].kind == blockTool && m.blocks[i].collapsed {
			t.Fatal("ctrl+o should expand all tool cards")
		}
	}
}

// drainShell consumes the live stream of a governed shell run the way the
// Bubble Tea loop would, answering any approval prompt with decide.
func drainShell(t *testing.T, m *operateModel, decide string) {
	t.Helper()
	if m.events == nil {
		t.Fatal("shell run did not start a stream")
	}
	for {
		ev, ok := <-m.events
		m.handleStream(opStreamMsg{ev: ev, ok: ok})
		if !ok {
			return
		}
		if m.pendingApproval != nil {
			m.handleApprovalKey(decide)
		}
	}
}

// assertSessionRecords fails unless the session transcript and tool-call audit
// both contain a record for tool.
func assertSessionRecords(t *testing.T, session *operate.Session, tool string) {
	t.Helper()
	transcript, err := os.ReadFile(session.TranscriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(transcript), `"tool_name":"`+tool+`"`) {
		t.Fatalf("transcript missing %s record:\n%s", tool, transcript)
	}
	audit, err := os.ReadFile(session.ToolCallsPath)
	if err != nil {
		t.Fatalf("read tool-calls audit: %v", err)
	}
	if !strings.Contains(string(audit), `"tool":"`+tool+`"`) {
		t.Fatalf("tool-calls audit missing %s record:\n%s", tool, audit)
	}
}

func TestBangCommandRunsShell(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	m.posture = operate.PostureAutoSafe
	m.submit("!echo hello-ouvrier")
	if !m.running {
		t.Fatal("governed shell command should stream like a turn")
	}
	drainShell(t, m, "enter")
	out := m.render()
	if !strings.Contains(out, "hello-ouvrier") {
		t.Fatalf("!cmd output not shown in transcript:\n%s", out)
	}
	// The governed shell leaves transcript + audit records.
	assertSessionRecords(t, m.session, "run_shell")
}

// Under the manual posture the `!` shell is gated: denial blocks execution and
// the denied call is still audited.
func TestBangCommandGatedUnderManualPosture(t *testing.T) {
	dir := t.TempDir()
	wdir := filepath.Join(dir, "demo")
	writeOperateWorker(t, wdir, "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: wdir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	if m.posture != operate.PostureManual {
		t.Fatalf("default posture = %q, want manual", m.posture)
	}
	m.submit("!touch denied-by-operator.txt")
	sawApproval := false
	for {
		ev, ok := <-m.events
		m.handleStream(opStreamMsg{ev: ev, ok: ok})
		if !ok {
			break
		}
		if m.pendingApproval != nil {
			sawApproval = true
			m.handleApprovalKey("esc")
		}
	}
	if !sawApproval {
		t.Fatal("manual posture must prompt before running a shell command")
	}
	if _, err := os.Stat(filepath.Join(wdir, "denied-by-operator.txt")); !os.IsNotExist(err) {
		t.Fatal("denied shell command must not execute")
	}
	// The denial itself is audited.
	audit, err := os.ReadFile(m.session.ToolCallsPath)
	if err != nil {
		t.Fatalf("read tool-calls audit: %v", err)
	}
	if !strings.Contains(string(audit), `"tool":"run_shell"`) || !strings.Contains(string(audit), "denied") {
		t.Fatalf("denied shell must leave an audit record:\n%s", audit)
	}
}

func TestBangBangCommandIsSilent(t *testing.T) {
	dir := t.TempDir()
	writeOperateWorker(t, filepath.Join(dir, "demo"), "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: filepath.Join(dir, "demo"), Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.posture = operate.PostureAutoSafe
	m.submit("!!echo secret-output")
	drainShell(t, m, "enter")
	out := m.render()
	if strings.Contains(out, "secret-output") {
		t.Fatalf("!!cmd output must be suppressed:\n%s", out)
	}
	// Silent only hides the UI blocks; transcript + audit records remain.
	assertSessionRecords(t, m.session, "run_shell")
}

func TestSlashClearStartsFreshTranscript(t *testing.T) {
	dir := t.TempDir()
	wdir := filepath.Join(dir, "demo")
	writeOperateWorker(t, wdir, "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: wdir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.blocks = append(m.blocks, opBlock{kind: blockUser, text: "old turn marker"})
	m.submit("/clear")
	for _, b := range m.blocks {
		if strings.Contains(b.text, "old turn marker") {
			t.Fatal("/clear should drop the previous transcript blocks")
		}
	}
}

func TestManualEditorOpenSaveReaudit(t *testing.T) {
	dir := t.TempDir()
	wdir := filepath.Join(dir, "demo")
	writeOperateWorker(t, wdir, "demo")
	m := newOperateModel(context.Background(), OperateOptions{Dir: wdir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	m.openEditor("main.go")
	if !m.showEditor || m.editorPath != "main.go" {
		t.Fatalf("editor not open: show=%v path=%q", m.showEditor, m.editorPath)
	}
	out := m.render()
	if !strings.Contains(out, "main.go") {
		t.Fatalf("editor overlay should show the path:\n%s", out)
	}
	m.editor.SetValue("package main\n\nfunc main() { /* edited */ }\n")
	if err := m.saveEditor(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(wdir, "main.go"))
	if !strings.Contains(string(data), "edited") {
		t.Fatalf("file not saved with new content: %s", data)
	}
	// The manual editor save is governed: transcript + audit records exist.
	assertSessionRecords(t, m.session, "write_worker_file")
}

func TestCockpitCtrlGOpensIDE(t *testing.T) {
	dir := t.TempDir()
	wdir := filepath.Join(dir, "demo")
	writeOperateWorker(t, wdir, "demo")

	m := newOperateModel(context.Background(), OperateOptions{Dir: wdir, Agent: "manual", Driver: operate.ManualDriver{}}).(*operateModel)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// ctrl+g should open the IDE.
	result, _ := m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	updated := result.(*operateModel)
	if !updated.ideActive {
		t.Fatal("ideActive should be true after ctrl+g")
	}
	if updated.ideModel == nil {
		t.Fatal("ideModel should not be nil after ctrl+g")
	}

	// Sending ExitMsg should return to the cockpit.
	result2, _ := updated.Update(ide.ExitMsg{})
	returned := result2.(*operateModel)
	if returned.ideActive {
		t.Fatal("ideActive should be false after ExitMsg")
	}
	if returned.ideModel != nil {
		t.Fatal("ideModel should be nil after ExitMsg")
	}
}

func writeOperateWorker(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir worker: %v", err)
	}
	write := func(file, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
	write("pip.yaml", "name: "+name+"\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	write("ouvrier.worker.json", `{"name":"`+name+`","events":["POST /tickets"],"outcomes":["triage"]}`+"\n")
}
