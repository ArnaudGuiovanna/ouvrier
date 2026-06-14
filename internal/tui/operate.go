package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

// OperateOptions configure the v0.4 local builder cockpit.
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
}

type operateModel struct {
	ctx     context.Context
	opts    OperateOptions
	width   int
	height  int
	runtime *operate.AgentRuntime
	session *operate.Session

	workspace  operate.Workspace
	candidates []operate.Workspace
	transcript []operate.TranscriptEntry
	input      textinput.Model
	err        error
	mode       string
	running    string
	log        []string
}

type operatePromptDone struct {
	turn operate.RuntimeTurn
	err  error
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
	input := newOperateInput()
	ws, _ := operate.DetectWorkspace(opts.Dir)
	model := &operateModel{
		ctx:       ctx,
		opts:      opts,
		width:     100,
		height:    32,
		workspace: ws,
		err:       nil,
		mode:      "operate",
		input:     input,
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
	})
	if err != nil {
		model.err = err
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
		return model
	}
	model.session = started.Session
	model.transcript = started.Transcript
	if started.Workspace != nil {
		model.workspace = *started.Workspace
	}
	if started.Workspace == nil && ws.Dir == "" {
		model.candidates = detectOperateCandidates(opts.Dir)
		if len(model.candidates) > 0 {
			model.mode = "select"
			model.log = append(model.log, "select a worker with 1-9 or prompt a new one")
		} else {
			model.mode = "factory"
		}
	}
	model.log = append(model.log, "session "+started.Session.ID+" ready")
	return model
}

func newOperateInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.Placeholder = "Create a worker, review this code, /audit, /build, /deploy staging..."
	ti.CharLimit = 4096
	ti.SetWidth(88)
	styles := ti.Styles()
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	faint := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex)).Faint(true)
	styles.Focused.Prompt = accent
	styles.Focused.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
	styles.Focused.Placeholder = faint
	styles.Blurred.Prompt = faint
	styles.Blurred.Text = faint
	styles.Blurred.Placeholder = faint
	ti.SetStyles(styles)
	return ti
}

func (m *operateModel) Init() tea.Cmd { return m.input.Focus() }

func (m *operateModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case operatePromptDone:
		m.running = ""
		if msg.err != nil {
			m.log = appendBounded(m.log, msg.err.Error())
		}
		if len(msg.turn.Entries) > 0 {
			m.transcript = append(m.transcript, msg.turn.Entries...)
		}
		if msg.turn.Workspace != nil {
			m.workspace = *msg.turn.Workspace
			m.mode = "operate"
			m.candidates = nil
		}
		if msg.turn.Final != "" {
			m.log = appendBounded(m.log, msg.turn.Final)
		}
	case tea.KeyPressMsg:
		if m.mode == "select" && len(m.candidates) > 0 && m.input.Value() == "" {
			if selected, ok := candidateIndex(msg.String()); ok {
				return m.selectCandidate(selected)
			}
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d", "esc":
			return m, tea.Quit
		case "enter":
			value := m.input.Value()
			if strings.TrimSpace(value) == "" {
				return m, nil
			}
			m.input.SetValue("")
			return m.startPrompt(value)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
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
	})
	if err != nil {
		m.err = err
		return m, nil
	}
	started, err := runtime.Start(m.ctx, operate.RuntimeStartRequest{
		Dir:           ws.Dir,
		InitialPrompt: m.opts.Goal,
		DriverID:      m.opts.Agent,
		CodexMode:     m.opts.CodexMode,
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
	m.transcript = started.Transcript
	m.candidates = nil
	m.err = nil
	m.mode = "operate"
	m.log = appendBounded(m.log, "session "+started.Session.ID+" ready")
	return m, nil
}

func (m *operateModel) startPrompt(prompt string) (*operateModel, tea.Cmd) {
	if m.err != nil {
		m.log = appendBounded(m.log, "prompt: "+m.err.Error())
		return m, nil
	}
	if m.runtime == nil || m.session == nil {
		m.log = appendBounded(m.log, "prompt: no active operate session")
		return m, nil
	}
	if m.running != "" {
		m.log = appendBounded(m.log, "prompt: "+m.running+" already running")
		return m, nil
	}
	m.running = "prompt"
	return m, func() tea.Msg {
		turn, err := m.runtime.Prompt(m.ctx, m.session.ID, prompt)
		return operatePromptDone{turn: turn, err: err}
	}
}

func candidateIndex(key string) (int, bool) {
	if len(key) != 1 || key[0] < '1' || key[0] > '9' {
		return 0, false
	}
	return int(key[0] - '1'), true
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

func (m *operateModel) View() tea.View {
	view := tea.NewView(renderOperate(m))
	view.AltScreen = true
	view.BackgroundColor = backgroundColor
	view.ForegroundColor = foregroundColor
	view.WindowTitle = "Ouvrier operate"
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
