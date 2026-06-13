package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

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
	harness *operate.Harness
	session *operate.Session

	workspace  operate.Workspace
	candidates []operate.Workspace
	err        error
	mode       string
	running    string
	log        []string
}

type operateActionDone struct {
	action  string
	lines   []string
	session *operate.Session
	err     error
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
	ws, err := operate.DetectWorkspace(opts.Dir)
	model := &operateModel{
		ctx:       ctx,
		opts:      opts,
		width:     100,
		height:    32,
		workspace: ws,
		err:       err,
		mode:      "review",
	}
	if err != nil {
		model.candidates = detectOperateCandidates(opts.Dir)
		if len(model.candidates) > 0 {
			model.err = nil
			model.mode = "select"
			model.log = append(model.log, "select a worker with 1-9")
		}
		return model
	}
	h, err := operate.NewHarness(operate.Options{Dir: ws.Dir, Driver: opts.Driver})
	if err != nil {
		model.err = err
		return model
	}
	model.harness = h
	if opts.Session != "" {
		session, err := h.Store.Load(opts.Session)
		if err != nil {
			model.err = err
			return model
		}
		model.session = session
		model.log = append(model.log, "session "+session.ID+" resumed")
		return model
	}
	session, ws, err := h.Start(ctx, ws.Dir, opts.Goal, opts.Agent, opts.CodexMode)
	if err != nil {
		model.err = err
		return model
	}
	model.workspace = ws
	model.session = session
	model.log = append(model.log, "session "+session.ID+" ready")
	return model
}

func (m *operateModel) Init() tea.Cmd { return nil }

func (m *operateModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case operateActionDone:
		m.running = ""
		if msg.session != nil {
			m.session = msg.session
		}
		if msg.err != nil {
			m.log = appendBounded(m.log, msg.action+": "+msg.err.Error())
			return m, nil
		}
		m.log = appendBounded(m.log, msg.lines...)
	case tea.KeyPressMsg:
		if m.mode == "select" && len(m.candidates) > 0 {
			if selected, ok := candidateIndex(msg.String()); ok {
				return m.selectCandidate(selected)
			}
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d", "esc", "q":
			return m, tea.Quit
		case "p":
			m.mode = "patch"
			return m.startAction("patch", m.patchCmd())
		case "r":
			m.mode = "review"
			return m.startAction("review", m.reviewCmd())
		case "f":
			m.mode = "fix"
			return m.startAction("fix", m.fixCmd())
		case "a":
			m.mode = "audit"
			return m.startAction("audit", m.auditCmd())
		case "b":
			m.mode = "build"
			return m.startAction("build", m.buildCmd())
		case "t":
			m.mode = "transfer"
			return m.startAction("transfer", m.transferCmd())
		}
	}
	return m, nil
}

func (m *operateModel) selectCandidate(index int) (*operateModel, tea.Cmd) {
	if index < 0 || index >= len(m.candidates) {
		return m, nil
	}
	ws := m.candidates[index]
	h, err := operate.NewHarness(operate.Options{Dir: ws.Dir, Driver: m.opts.Driver})
	if err != nil {
		m.err = err
		return m, nil
	}
	session, ws, err := h.Start(m.ctx, ws.Dir, m.opts.Goal, m.opts.Agent, m.opts.CodexMode)
	if err != nil {
		m.err = err
		return m, nil
	}
	m.harness = h
	m.session = session
	m.workspace = ws
	m.candidates = nil
	m.err = nil
	m.mode = "review"
	m.log = appendBounded(m.log, "session "+session.ID+" ready")
	return m, nil
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
