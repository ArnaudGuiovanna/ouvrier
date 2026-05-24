package tui

import (
	"context"
	"errors"
	"io"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/provider"
	"github.com/ArnaudGuiovanna/ouvrier/internal/scaffold"
)

// wizardStep identifies the four wizard panels.
type wizardStep int

const (
	stepName wizardStep = iota
	stepTrigger
	stepModel
	stepReview
	stepDone
)

// GenerateFunc is the function-typed seam used to run scaffold.Generate so that
// tests can substitute a fake project generator without touching the file
// system.
type GenerateFunc func(ctx context.Context, cfg scaffold.Config) (scaffold.Project, error)

// defaultModel is the model id pre-populated in the model step.
const defaultModel = "anthropic/claude-sonnet-4-6"

// wizardModel is the interactive Bubble Tea model that backs `ouvrier new`.
// It owns three text inputs (name, trigger, model) plus a review screen that
// invokes scaffold.Generate when the operator confirms.
type wizardModel struct {
	step wizardStep

	width  int
	height int

	parentDir       string
	frameworkDir    string
	frameworkModule string

	nameInput    textinput.Model
	triggerInput textinput.Model
	modelInput   textinput.Model

	errMsg string

	generate GenerateFunc

	cancelled bool
	project   *scaffold.Project
	genErr    error
}

// NewProjectWizardOptions configures the interactive wizard.
type NewProjectWizardOptions struct {
	ParentDir       string
	FrameworkDir    string
	FrameworkModule string
	Generate        GenerateFunc
}

// newWizardModel constructs the wizard state with sensible defaults.
func newWizardModel(opts NewProjectWizardOptions) *wizardModel {
	parent := strings.TrimSpace(opts.ParentDir)
	if parent == "" {
		parent = "."
	}
	gen := opts.Generate
	if gen == nil {
		gen = scaffold.Generate
	}

	accent := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	faint := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex)).Faint(true)

	mk := func(placeholder, value string) textinput.Model {
		ti := textinput.New()
		ti.Prompt = "> "
		ti.Placeholder = placeholder
		ti.CharLimit = 128
		ti.SetWidth(48)
		styles := ti.Styles()
		styles.Focused.Prompt = accent
		styles.Focused.Text = lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
		styles.Focused.Placeholder = faint
		styles.Blurred.Prompt = faint
		styles.Blurred.Text = faint
		styles.Blurred.Placeholder = faint
		ti.SetStyles(styles)
		if value != "" {
			ti.SetValue(value)
			ti.CursorEnd()
		}
		return ti
	}

	m := &wizardModel{
		step:            stepName,
		width:           80,
		height:          24,
		parentDir:       parent,
		frameworkDir:    opts.FrameworkDir,
		frameworkModule: opts.FrameworkModule,
		nameInput:       mk("my-worker", ""),
		triggerInput:    mk("POST /tickets", ""),
		modelInput:      mk(defaultModel, defaultModel),
		generate:        gen,
	}
	return m
}

// NewProjectWizard returns a Bubble Tea model implementing the four-step
// wizard. It is exposed for tests and embedders that want to drive the wizard
// themselves.
func NewProjectWizard(opts NewProjectWizardOptions) tea.Model {
	return newWizardModel(opts)
}

func (m *wizardModel) Init() tea.Cmd {
	return m.nameInput.Focus()
}

func (m *wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// Forward other messages (cursor blink, paste, etc.) to the focused input.
	switch m.step {
	case stepName:
		var cmd tea.Cmd
		m.nameInput, cmd = m.nameInput.Update(msg)
		return m, cmd
	case stepTrigger:
		var cmd tea.Cmd
		m.triggerInput, cmd = m.triggerInput.Update(msg)
		return m, cmd
	case stepModel:
		var cmd tea.Cmd
		m.modelInput, cmd = m.modelInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *wizardModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global quit shortcuts. We intentionally do not accept plain "q" while
	// the user is typing into an input – it would clobber legitimate input.
	switch key {
	case "ctrl+c", "esc":
		m.cancelled = true
		return m, tea.Quit
	}

	switch m.step {
	case stepReview:
		return m.handleReviewKey(key)
	case stepDone:
		// After Generate finished, any key dismisses the wizard.
		return m, tea.Quit
	}

	switch key {
	case "enter":
		return m.advance()
	case "tab":
		return m.advance()
	case "shift+tab":
		return m.retreat()
	}

	// Forward all other key presses to the focused input.
	var cmd tea.Cmd
	switch m.step {
	case stepName:
		m.nameInput, cmd = m.nameInput.Update(msg)
	case stepTrigger:
		m.triggerInput, cmd = m.triggerInput.Update(msg)
	case stepModel:
		m.modelInput, cmd = m.modelInput.Update(msg)
	}
	// Editing clears the inline error – the operator is fixing it.
	m.errMsg = ""
	return m, cmd
}

func (m *wizardModel) handleReviewKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "y", "Y", "enter":
		project, err := m.generate(context.Background(), m.buildConfig())
		if err != nil {
			m.genErr = err
			m.errMsg = err.Error()
			// Send the operator back to the offending step where possible.
			m.step = stepName
			return m, m.nameInput.Focus()
		}
		m.project = &project
		m.step = stepDone
		return m, tea.Quit
	case "n", "N", "q":
		m.cancelled = true
		return m, tea.Quit
	case "shift+tab", "backspace":
		return m.retreat()
	}
	return m, nil
}

func (m *wizardModel) advance() (tea.Model, tea.Cmd) {
	switch m.step {
	case stepName:
		name := strings.TrimSpace(m.nameInput.Value())
		if !scaffold.ValidProjectName(name) {
			m.errMsg = "project name must use letters, digits, '-', '_' or '.' and have no path separators"
			return m, nil
		}
		m.nameInput.SetValue(name)
		m.nameInput.Blur()
		m.step = stepTrigger
		m.errMsg = ""
		return m, m.triggerInput.Focus()
	case stepTrigger:
		trigger := strings.TrimSpace(m.triggerInput.Value())
		normalized, err := scaffold.NormalizeHTTPTrigger(trigger)
		if err != nil {
			m.errMsg = humanError(err)
			return m, nil
		}
		m.triggerInput.SetValue(normalized)
		m.triggerInput.Blur()
		m.step = stepModel
		m.errMsg = ""
		return m, m.modelInput.Focus()
	case stepModel:
		model := strings.TrimSpace(m.modelInput.Value())
		if _, err := provider.ParseModelID(model); err != nil {
			m.errMsg = humanError(err)
			return m, nil
		}
		m.modelInput.SetValue(model)
		m.modelInput.Blur()
		m.step = stepReview
		m.errMsg = ""
		return m, nil
	}
	return m, nil
}

func (m *wizardModel) retreat() (tea.Model, tea.Cmd) {
	m.errMsg = ""
	switch m.step {
	case stepTrigger:
		m.triggerInput.Blur()
		m.step = stepName
		return m, m.nameInput.Focus()
	case stepModel:
		m.modelInput.Blur()
		m.step = stepTrigger
		return m, m.triggerInput.Focus()
	case stepReview:
		m.step = stepModel
		return m, m.modelInput.Focus()
	}
	return m, nil
}

func (m *wizardModel) buildConfig() scaffold.Config {
	return scaffold.Config{
		Name:            strings.TrimSpace(m.nameInput.Value()),
		Trigger:         strings.TrimSpace(m.triggerInput.Value()),
		Model:           strings.TrimSpace(m.modelInput.Value()),
		Dir:             m.parentDir,
		FrameworkModule: m.frameworkModule,
		FrameworkDir:    m.frameworkDir,
	}
}

// View renders the wizard inside the existing Ouvrier panel/screen styles.
func (m *wizardModel) View() tea.View {
	view := tea.NewView(renderWizard(m))
	view.AltScreen = true
	view.BackgroundColor = backgroundColor
	view.ForegroundColor = foregroundColor
	view.WindowTitle = "Ouvrier new"
	return view
}

func renderWizard(m *wizardModel) string {
	width := clamp(m.width-8, 36, 72)

	titleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(greenHex)).
		Background(lipgloss.Color(blackHex)).
		Bold(true)
	mutedStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(offWhiteHex)).
		Faint(true)
	labelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(greenHex))
	valueStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(offWhiteHex))
	errorStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ff5f5f")).
		Bold(true)
	panelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(offWhiteHex)).
		Background(lipgloss.Color(blackHex)).
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(greenHex)).
		Padding(1, 2).
		Width(width)
	screenStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(offWhiteHex)).
		Background(lipgloss.Color(blackHex)).
		Padding(1, 2)

	lines := []string{
		titleStyle.Render("Ouvrier"),
		mutedStyle.Render("Workers for your APIs."),
		"",
		labelStyle.Render(stepHeading(m.step)),
		mutedStyle.Render(stepHint(m.step)),
		"",
	}

	switch m.step {
	case stepName:
		lines = append(lines,
			labelStyle.Render("Project name"),
			m.nameInput.View(),
		)
	case stepTrigger:
		lines = append(lines,
			labelStyle.Render("HTTP trigger"),
			m.triggerInput.View(),
			mutedStyle.Render("v0.1 supports HTTP only; e.g. \"POST /tickets\""),
		)
	case stepModel:
		lines = append(lines,
			labelStyle.Render("Model"),
			m.modelInput.View(),
			mutedStyle.Render("provider/name, e.g. anthropic/claude-sonnet-4-6"),
		)
	case stepReview:
		lines = append(lines,
			labelStyle.Render("Project name")+"  "+valueStyle.Render(m.nameInput.Value()),
			labelStyle.Render("Trigger     ")+"  "+valueStyle.Render(m.triggerInput.Value()),
			labelStyle.Render("Model       ")+"  "+valueStyle.Render(m.modelInput.Value()),
			labelStyle.Render("Parent dir  ")+"  "+valueStyle.Render(m.parentDir),
			"",
			valueStyle.Render("Generate project? [y/N]"),
		)
	case stepDone:
		if m.project != nil {
			lines = append(lines,
				labelStyle.Render("Created"),
				valueStyle.Render(m.project.Dir),
			)
		}
	}

	if m.errMsg != "" {
		lines = append(lines, "", errorStyle.Render("error: "+m.errMsg))
	}

	lines = append(lines, "", mutedStyle.Render(footerHint(m.step)))

	body := strings.Join(lines, "\n")
	return screenStyle.Render(panelStyle.Render(body))
}

func stepHeading(step wizardStep) string {
	switch step {
	case stepName:
		return "new project - step 1 of 4: name"
	case stepTrigger:
		return "new project - step 2 of 4: trigger"
	case stepModel:
		return "new project - step 3 of 4: model"
	case stepReview:
		return "new project - step 4 of 4: review"
	case stepDone:
		return "project generated"
	}
	return "new project"
}

func stepHint(step wizardStep) string {
	switch step {
	case stepName:
		return "letters, digits, '-', '_', '.' - no slashes"
	case stepTrigger:
		return "HTTP only in v0.1: METHOD /path"
	case stepModel:
		return "validated with provider.ParseModelID"
	case stepReview:
		return "y/Enter to generate, n/q to cancel"
	case stepDone:
		return "press any key to exit"
	}
	return ""
}

func footerHint(step wizardStep) string {
	switch step {
	case stepReview:
		return "y generate | n/q cancel | shift+tab back | esc/ctrl+c quit"
	case stepDone:
		return "any key to exit"
	}
	return "enter/tab next | shift+tab back | esc/ctrl+c quit"
}

// humanError trims noisy wrapper text from scaffold errors so the inline
// message stays short enough to fit on one line.
func humanError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// scaffold.ErrInvalidConfig surfaces as "invalid scaffold config: <reason>".
	if errors.Is(err, scaffold.ErrInvalidConfig) {
		if idx := strings.Index(msg, ": "); idx >= 0 {
			msg = msg[idx+2:]
		}
	}
	return msg
}

// RunNewProject drives the interactive wizard end-to-end. It returns the
// created project (nil if the operator cancelled) and any error surfaced by
// the Bubble Tea program or by scaffold.Generate.
func RunNewProject(in io.Reader, out io.Writer, opts NewProjectWizardOptions) (*scaffold.Project, error) {
	m := newWizardModel(opts)
	program := tea.NewProgram(
		m,
		tea.WithInput(in),
		tea.WithOutput(out),
	)
	if _, err := program.Run(); err != nil {
		return nil, err
	}
	if m.genErr != nil {
		return nil, m.genErr
	}
	if m.cancelled || m.project == nil {
		return nil, nil
	}
	return m.project, nil
}
