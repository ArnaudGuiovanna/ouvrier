package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

// OperateOptions configure the local Ouvrier worker-factory cockpit.
type OperateOptions struct {
	Dir       string
	Agent     string
	CodexMode string
	Session   string
	Goal      string
	Driver    operate.Driver
	Env       string
	EnvFile   string
	Target    string
	Keep      int
	AllowFail bool

	// Model enables the Ouvrier-owned model tool-calling loop; ModelID is the
	// provider/model id shown in the status bar and used for requests. When nil
	// the cockpit falls back to the deterministic keyword planner.
	Model   operate.AgentModel
	ModelID string
}

// blockKind classifies one rendered transcript block in the cockpit.
type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockTool
	blockNotice
	blockError
)

// opBlock is a single rendered unit in the scrolling transcript. Tool calls and
// their results are merged into one card so the transcript reads like Pi/Claude
// Code rather than a raw event log.
type opBlock struct {
	kind       blockKind
	text       string
	toolName   string
	toolInput  map[string]any
	toolOutput map[string]any
	toolErr    bool
	running    bool
	streaming  bool
}

type operateModel struct {
	ctx    context.Context
	opts   OperateOptions
	width  int
	height int
	ready  bool

	runtime    *operate.AgentRuntime
	session    *operate.Session
	workspace  operate.Workspace
	candidates []operate.Workspace
	mode       string // select | operate | factory
	err        error

	vp       viewport.Model
	composer textarea.Model
	spin     spinner.Model

	blocks         []opBlock
	running        bool
	cancel         context.CancelFunc
	events         <-chan operate.StreamEvent
	queue          []string
	runningToolIdx int
	startedAt      time.Time

	slashActive  bool
	slashMatches []slashCmd
	slashIndex   int

	models     []string
	modelIndex int

	showHelp bool
	status   string

	pendingApproval *operate.ApprovalRequest
	decisions       chan<- operate.ApprovalDecision
	posture         operate.Posture
	prodConfirm     string
}

// slashCmd is one accelerator surfaced in the composer's command menu.
type slashCmd struct {
	name  string
	usage string
	desc  string
}

var operateSlashCommands = []slashCmd{
	{"/new", "/new worker <name> --trigger \"POST /x\"", "Scaffold a new Ouvrier worker"},
	{"/review", "/review [scope]", "Read-only security & governance review"},
	{"/fix", "/fix [finding]", "Repair review/audit findings"},
	{"/audit", "/audit", "Run deterministic audit gates"},
	{"/diff", "/diff", "Show the candidate diff"},
	{"/build", "/build --target linux/amd64", "Compile the worker binary"},
	{"/deploy", "/deploy staging", "Build then transfer through the deploy engine"},
	{"/read", "/read main.go", "Read a worker file"},
	{"/docs", "/docs <query>", "Search the Ouvrier docs & API"},
	{"/workers", "/workers", "List detected workers"},
	{"/login", "/login codex", "Probe/delegate Codex authentication"},
	{"/accept-risk", "/accept-risk <rationale>", "Record an accepted risk"},
	{"/export", "/export", "Export the transcript to Markdown"},
	{"/tools", "/tools", "List the native Ouvrier tools"},
	{"/policy", "/policy", "Show the tool/safety policy"},
	{"/help", "/help", "Show cockpit help"},
}

type opStreamMsg struct {
	ev operate.StreamEvent
	ok bool
}

func waitStream(ch <-chan operate.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return opStreamMsg{ev: ev, ok: ok}
	}
}

// NewOperateModel returns the Bubble Tea model for `ouvrier operate`.
func NewOperateModel(opts OperateOptions) tea.Model {
	return newOperateModel(context.Background(), opts)
}

func newOperateModel(ctx context.Context, opts OperateOptions) tea.Model {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.Agent == "" {
		opts.Agent = "codex"
	}
	if opts.CodexMode == "" {
		opts.CodexMode = "auto"
	}
	if opts.Driver == nil {
		opts.Driver = operate.ManualDriver{}
	}

	ws, _ := operate.DetectWorkspace(opts.Dir)
	model := &operateModel{
		ctx:            ctx,
		opts:           opts,
		width:          100,
		height:         32,
		workspace:      ws,
		mode:           "operate",
		composer:       newComposer(),
		spin:           newSpinner(),
		vp:             viewport.New(),
		runningToolIdx: -1,
		models:         defaultModelChoices(opts),
		posture:        operate.PostureManual,
	}

	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{
		Dir:       opts.Dir,
		Driver:    opts.Driver,
		DriverID:  opts.Agent,
		CodexMode: opts.CodexMode,
		Env:       opts.Env,
		EnvFile:   opts.EnvFile,
		Target:    opts.Target,
		Keep:      opts.Keep,
		AllowFail: opts.AllowFail,
		Model:     opts.Model,
		ModelID:   opts.ModelID,
	})
	if err != nil {
		model.err = err
		model.resize()
		return model
	}
	model.runtime = runtime

	started, err := runtime.Start(ctx, operate.RuntimeStartRequest{
		Dir:           opts.Dir,
		SessionID:     opts.Session,
		InitialPrompt: opts.Goal,
		DriverID:      opts.Agent,
		CodexMode:     opts.CodexMode,
	})
	if err != nil {
		model.err = err
		model.resize()
		return model
	}
	model.session = started.Session
	model.blocks = blocksFromTranscript(started.Transcript)
	if started.Workspace != nil {
		model.workspace = *started.Workspace
	}
	if started.Workspace == nil && ws.Dir == "" {
		model.candidates = detectOperateCandidates(opts.Dir)
		if len(model.candidates) > 0 {
			model.mode = "select"
		} else {
			model.mode = "factory"
		}
	}
	model.resize()
	model.refreshViewport()
	return model
}

func newComposer() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Describe a worker to build, or run /review, /audit, /build, /deploy…"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 6
	// Enter submits the prompt; newlines are inserted explicitly.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	styles := ta.Styles()
	styles.Focused.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	styles.Focused.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
	styles.Focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	styles.Blurred.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	ta.SetStyles(styles)
	ta.Focus()
	return ta
}

func newSpinner() spinner.Model {
	s := spinner.New()
	s.Spinner = spinner.MiniDot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	return s
}

func defaultModelChoices(opts OperateOptions) []string {
	if strings.TrimSpace(opts.ModelID) != "" {
		return []string{opts.ModelID}
	}
	if strings.EqualFold(opts.Agent, "manual") {
		return []string{"manual"}
	}
	return []string{
		"codex/gpt-5.5 high",
		"codex/gpt-5.5 medium",
		"anthropic/claude-sonnet-4-6",
		"openai/gpt-5.5",
	}
}

func (m *operateModel) Init() tea.Cmd { return m.composer.Focus() }

func (m *operateModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resize()
		m.refreshViewport()
		return m, nil
	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		m.refreshViewport()
		return m, cmd
	case opStreamMsg:
		return m.handleStream(msg)
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	return m, cmd
}

func (m *operateModel) handleStream(msg opStreamMsg) (tea.Model, tea.Cmd) {
	if !msg.ok {
		m.running = false
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.events = nil
		m.runningToolIdx = -1
		m.refreshViewport()
		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			return m.startTurn(next)
		}
		m.status = ""
		return m, nil
	}
	m.applyStream(msg.ev)
	m.refreshViewport()
	return m, waitStream(m.events)
}

func (m *operateModel) applyStream(ev operate.StreamEvent) {
	switch ev.Kind {
	case operate.StreamUser:
		// Already echoed locally on submit.
	case operate.StreamStatus:
		if strings.TrimSpace(ev.Final) != "" {
			m.blocks = append(m.blocks, opBlock{kind: blockAssistant, text: ev.Final})
		}
	case operate.StreamAssistantDelta:
		m.appendDelta(ev.Delta)
	case operate.StreamAssistant:
		m.finalizeAssistant(ev)
	case operate.StreamToolStart:
		b := opBlock{kind: blockTool, running: true}
		if ev.Entry != nil {
			b.toolName = ev.Entry.ToolName
			b.toolInput = ev.Entry.Input
		}
		m.blocks = append(m.blocks, b)
		m.runningToolIdx = len(m.blocks) - 1
	case operate.StreamToolEnd:
		if m.runningToolIdx >= 0 && m.runningToolIdx < len(m.blocks) {
			b := &m.blocks[m.runningToolIdx]
			b.running = false
			b.toolErr = ev.Err != nil
			if ev.Entry != nil {
				b.toolOutput = ev.Entry.Output
				if b.toolName == "" {
					b.toolName = ev.Entry.ToolName
				}
				if _, ok := ev.Entry.Output["error"]; ok {
					b.toolErr = true
				}
			}
		}
		m.runningToolIdx = -1
	case operate.StreamApproval:
		m.pendingApproval = ev.Approval
		m.prodConfirm = ""
	case operate.StreamError:
		if ev.Entry == nil {
			return
		}
		// A failed tool already renders its error inside the card; only add a
		// standalone error block for non-tool failures.
		if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == blockTool && m.blocks[n-1].toolErr {
			return
		}
		m.blocks = append(m.blocks, opBlock{kind: blockError, text: ev.Entry.Text})
	case operate.StreamDone:
		m.pendingApproval = nil
		m.prodConfirm = ""
		if ev.Workspace != nil {
			m.workspace = *ev.Workspace
			if m.mode != "operate" {
				m.mode = "operate"
				m.candidates = nil
			}
		}
	}
}

func (m *operateModel) appendDelta(delta string) {
	if delta == "" {
		return
	}
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == blockAssistant && m.blocks[n-1].streaming {
		m.blocks[n-1].text += delta
		return
	}
	m.blocks = append(m.blocks, opBlock{kind: blockAssistant, text: delta, streaming: true})
}

func (m *operateModel) finalizeAssistant(ev operate.StreamEvent) {
	text := ""
	if ev.Entry != nil {
		text = ev.Entry.Text
	}
	if n := len(m.blocks); n > 0 && m.blocks[n-1].kind == blockAssistant && m.blocks[n-1].streaming {
		m.blocks[n-1].streaming = false
		if text != "" {
			m.blocks[n-1].text = text
		}
		return
	}
	if text != "" {
		m.blocks = append(m.blocks, opBlock{kind: blockAssistant, text: text})
	}
}

func (m *operateModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	keyStr := msg.String()

	if keyStr == "ctrl+c" {
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	}
	if m.showHelp {
		m.showHelp = false
		m.resize()
		return m, nil
	}

	if m.pendingApproval != nil {
		return m.handleApprovalKey(keyStr)
	}

	// Preserve the worker selection shortcut from the parent-directory factory.
	if m.mode == "select" && len(m.candidates) > 0 && strings.TrimSpace(m.composer.Value()) == "" {
		if idx, ok := candidateIndex(keyStr); ok {
			return m.selectCandidate(idx)
		}
	}

	switch keyStr {
	case "esc":
		if m.slashActive {
			m.slashActive = false
			m.resize()
			return m, nil
		}
		if m.running && m.cancel != nil {
			m.cancel()
			m.status = "interrupting…"
			return m, nil
		}
		return m, nil
	case "ctrl+p":
		m.cycleModel()
		return m, nil
	case "?":
		if strings.TrimSpace(m.composer.Value()) == "" {
			m.showHelp = true
			return m, nil
		}
	case "pgup":
		m.vp.ScrollUp(m.vp.Height() / 2)
		return m, nil
	case "pgdown":
		m.vp.ScrollDown(m.vp.Height() / 2)
		return m, nil
	case "ctrl+u":
		m.vp.ScrollUp(m.vp.Height() / 2)
		return m, nil
	case "ctrl+d":
		m.vp.ScrollDown(m.vp.Height() / 2)
		return m, nil
	case "up":
		if m.slashActive {
			if m.slashIndex > 0 {
				m.slashIndex--
			}
			return m, nil
		}
	case "down":
		if m.slashActive {
			if m.slashIndex < len(m.slashMatches)-1 {
				m.slashIndex++
			}
			return m, nil
		}
	case "tab":
		if m.slashActive && len(m.slashMatches) > 0 {
			m.completeSlash()
			return m, nil
		}
	case "shift+tab":
		m.posture = nextPosture(m.posture)
		return m, nil
	case "enter":
		if m.slashActive && len(m.slashMatches) > 0 {
			name := m.slashMatches[m.slashIndex].name
			m.composer.Reset()
			m.slashActive = false
			m.resize()
			return m.submit(name)
		}
		val := m.composer.Value()
		m.composer.Reset()
		m.slashActive = false
		m.resize()
		return m.submit(val)
	}

	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	m.updateSlash()
	m.resize()
	return m, cmd
}

func (m *operateModel) submit(text string) (tea.Model, tea.Cmd) {
	text = strings.TrimSpace(text)
	if text == "" {
		return m, nil
	}
	if m.err != nil {
		m.blocks = append(m.blocks, opBlock{kind: blockError, text: m.err.Error()})
		m.refreshViewport()
		return m, nil
	}
	if m.runtime == nil || m.session == nil {
		m.blocks = append(m.blocks, opBlock{kind: blockError, text: "no active operate session"})
		m.refreshViewport()
		return m, nil
	}
	if m.running {
		m.queue = append(m.queue, text)
		m.status = fmt.Sprintf("queued follow-up (%d pending)", len(m.queue))
		m.blocks = append(m.blocks, opBlock{kind: blockUser, text: text})
		m.refreshViewport()
		return m, nil
	}
	return m.startTurn(text)
}

func (m *operateModel) startTurn(text string) (tea.Model, tea.Cmd) {
	m.blocks = append(m.blocks, opBlock{kind: blockUser, text: text})
	ctx, cancel := context.WithCancel(m.ctx)
	ch, dec, err := m.runtime.RunTurnInteractive(ctx, m.session.ID, text, "prompt", m.posture)
	if err != nil {
		cancel()
		m.blocks = append(m.blocks, opBlock{kind: blockError, text: err.Error()})
		m.refreshViewport()
		return m, nil
	}
	m.running = true
	m.runningToolIdx = -1
	m.cancel = cancel
	m.events = ch
	m.decisions = dec
	m.startedAt = time.Now()
	m.status = ""
	m.refreshViewport()
	return m, tea.Batch(waitStream(ch), m.spin.Tick)
}

func (m *operateModel) selectCandidate(index int) (*operateModel, tea.Cmd) {
	if index < 0 || index >= len(m.candidates) {
		return m, nil
	}
	ws := m.candidates[index]
	runtime, err := operate.NewAgentRuntime(operate.RuntimeOptions{
		Dir:       ws.Dir,
		Driver:    m.opts.Driver,
		DriverID:  m.opts.Agent,
		CodexMode: m.opts.CodexMode,
		Env:       m.opts.Env,
		EnvFile:   m.opts.EnvFile,
		Target:    m.opts.Target,
		Keep:      m.opts.Keep,
		AllowFail: m.opts.AllowFail,
		Model:     m.opts.Model,
		ModelID:   m.opts.ModelID,
	})
	if err != nil {
		m.err = err
		return m, nil
	}
	started, err := runtime.Start(m.ctx, operate.RuntimeStartRequest{
		Dir:       ws.Dir,
		DriverID:  m.opts.Agent,
		CodexMode: m.opts.CodexMode,
	})
	if err != nil {
		m.err = err
		return m, nil
	}
	m.runtime = runtime
	m.session = started.Session
	if started.Workspace != nil {
		m.workspace = *started.Workspace
	} else {
		m.workspace = ws
	}
	m.blocks = blocksFromTranscript(started.Transcript)
	m.candidates = nil
	m.err = nil
	m.mode = "operate"
	m.resize()
	m.refreshViewport()
	return m, nil
}

func (m *operateModel) cycleModel() {
	if len(m.models) == 0 {
		return
	}
	m.modelIndex = (m.modelIndex + 1) % len(m.models)
}

func (m *operateModel) currentModel() string {
	if len(m.models) == 0 {
		return m.opts.Agent
	}
	return m.models[m.modelIndex]
}

func (m *operateModel) updateSlash() {
	val := m.composer.Value()
	if strings.HasPrefix(val, "/") && !strings.Contains(val, "\n") && !strings.Contains(strings.TrimSpace(val), " ") {
		m.slashMatches = filterSlash(val)
		m.slashActive = len(m.slashMatches) > 0
		if m.slashIndex >= len(m.slashMatches) {
			m.slashIndex = 0
		}
		return
	}
	m.slashActive = false
	m.slashMatches = nil
	m.slashIndex = 0
}

func (m *operateModel) completeSlash() {
	if len(m.slashMatches) == 0 {
		return
	}
	m.composer.SetValue(m.slashMatches[m.slashIndex].name + " ")
	m.slashActive = false
	m.updateSlash()
	m.resize()
}

func filterSlash(prefix string) []slashCmd {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	var out []slashCmd
	for _, cmd := range operateSlashCommands {
		if prefix == "/" || strings.HasPrefix(cmd.name, prefix) {
			out = append(out, cmd)
		}
	}
	return out
}

func (m *operateModel) resize() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	m.ready = true
	cw := max(m.width-2, 40)
	lines := strings.Count(m.composer.Value(), "\n") + 1
	ch := clamp(lines, 1, 6)
	m.composer.SetWidth(cw)
	m.composer.SetHeight(ch)

	slashH := 0
	if m.slashActive {
		slashH = clamp(len(m.slashMatches), 1, 6)
	}
	// rule(1) + composer(ch) + status(1) + hints(1) + slash menu.
	used := 1 + ch + 1 + 1 + slashH
	vh := max(m.height-used, 3)
	m.vp.SetWidth(cw)
	m.vp.SetHeight(vh)
}

func (m *operateModel) refreshViewport() {
	if !m.ready {
		return
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(m.renderTranscript())
	if atBottom || m.running {
		m.vp.GotoBottom()
	}
}

func candidateIndex(keyStr string) (int, bool) {
	if len(keyStr) != 1 || keyStr[0] < '1' || keyStr[0] > '9' {
		return 0, false
	}
	return int(keyStr[0] - '1'), true
}

func detectOperateCandidates(dir string) []operate.Workspace {
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var candidates []operate.Workspace
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ws, err := operate.DetectWorkspace(filepath.Join(dir, entry.Name()))
		if err == nil {
			candidates = append(candidates, ws)
		}
		if len(candidates) == 9 {
			break
		}
	}
	return candidates
}

func blocksFromTranscript(entries []operate.TranscriptEntry) []opBlock {
	var blocks []opBlock
	lastTool := -1
	for _, e := range entries {
		switch e.Kind {
		case operate.TranscriptUser:
			blocks = append(blocks, opBlock{kind: blockUser, text: e.Text})
			lastTool = -1
		case operate.TranscriptAssistant:
			if strings.TrimSpace(e.Text) != "" {
				blocks = append(blocks, opBlock{kind: blockAssistant, text: e.Text})
			}
			lastTool = -1
		case operate.TranscriptToolCall:
			blocks = append(blocks, opBlock{kind: blockTool, toolName: e.ToolName, toolInput: e.Input})
			lastTool = len(blocks) - 1
		case operate.TranscriptToolResult:
			if lastTool >= 0 && lastTool < len(blocks) && blocks[lastTool].toolName == e.ToolName {
				blocks[lastTool].toolOutput = e.Output
				if _, ok := e.Output["error"]; ok {
					blocks[lastTool].toolErr = true
				}
			} else {
				b := opBlock{kind: blockTool, toolName: e.ToolName, toolOutput: e.Output}
				if _, ok := e.Output["error"]; ok {
					b.toolErr = true
				}
				blocks = append(blocks, b)
			}
			lastTool = -1
		case operate.TranscriptStatus:
			blocks = append(blocks, opBlock{kind: blockNotice, text: e.Text})
			lastTool = -1
		case operate.TranscriptError:
			blocks = append(blocks, opBlock{kind: blockError, text: e.Text})
			lastTool = -1
		}
	}
	return blocks
}

func (m *operateModel) handleApprovalKey(keyStr string) (tea.Model, tea.Cmd) {
	ap := m.pendingApproval
	deny := func() (tea.Model, tea.Cmd) {
		if m.decisions != nil {
			m.decisions <- operate.ApprovalDecision{ID: ap.ID, Approved: false, Reason: "denied by operator"}
		}
		m.pendingApproval = nil
		m.prodConfirm = ""
		return m, nil
	}
	approve := func() (tea.Model, tea.Cmd) {
		if m.decisions != nil {
			m.decisions <- operate.ApprovalDecision{ID: ap.ID, Approved: true}
		}
		m.pendingApproval = nil
		m.prodConfirm = ""
		return m, nil
	}
	switch keyStr {
	case "esc":
		return deny()
	}
	if ap.Prod {
		want := workerNameForApproval(ap)
		switch keyStr {
		case "enter":
			if want != "" && m.prodConfirm == want {
				return approve()
			}
			return m, nil
		case "backspace":
			if r := []rune(m.prodConfirm); len(r) > 0 {
				m.prodConfirm = string(r[:len(r)-1])
			}
			return m, nil
		default:
			if len(keyStr) == 1 {
				m.prodConfirm += keyStr
			}
			return m, nil
		}
	}
	switch keyStr {
	case "enter", "y":
		return approve()
	case "n":
		return deny()
	}
	return m, nil
}

func workerNameForApproval(ap *operate.ApprovalRequest) string {
	if ap == nil || ap.Details == nil {
		return ""
	}
	if s, ok := ap.Details["worker"].(string); ok {
		return s
	}
	return ""
}

func nextPosture(p operate.Posture) operate.Posture {
	switch p {
	case operate.PostureManual:
		return operate.PostureAutoSafe
	case operate.PostureAutoSafe:
		return operate.PosturePlan
	default:
		return operate.PostureManual
	}
}

func (m *operateModel) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.BackgroundColor = backgroundColor
	view.ForegroundColor = foregroundColor
	view.WindowTitle = "ouvrier operate"
	return view
}

// RunOperate drives the local operate cockpit until the user exits.
func RunOperate(ctx context.Context, in io.Reader, out io.Writer, opts OperateOptions) error {
	if opts.Driver != nil {
		defer opts.Driver.Close()
	}
	program := tea.NewProgram(
		newOperateModel(ctx, opts),
		tea.WithInput(in),
		tea.WithOutput(out),
	)
	_, err := program.Run()
	return err
}
