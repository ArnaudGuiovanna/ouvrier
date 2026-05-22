package tui

import (
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/lipgloss"
)

type newProjectModel struct {
	width  int
	height int
}

func NewProjectModel() tea.Model {
	return newProjectModel{
		width:  80,
		height: 24,
	}
}

func RunNewProject(in io.Reader, out io.Writer) error {
	program := tea.NewProgram(
		NewProjectModel(),
		tea.WithInput(in),
		tea.WithOutput(out),
	)
	_, err := program.Run()
	return err
}

func (m newProjectModel) Init() tea.Cmd {
	return nil
}

func (m newProjectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m newProjectModel) View() tea.View {
	view := tea.NewView(renderNewProject(m))
	view.AltScreen = true
	view.BackgroundColor = backgroundColor
	view.ForegroundColor = foregroundColor
	view.WindowTitle = "Ouvrier new"
	return view
}

func renderNewProject(m newProjectModel) string {
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

	body := strings.Join([]string{
		titleStyle.Render("Ouvrier"),
		mutedStyle.Render("Workers for your APIs."),
		"",
		labelStyle.Render("new project"),
		mutedStyle.Render("preview only - use --yes flags to scaffold"),
		"",
		"Project name",
		"Trigger",
		"Pipeline agents",
		"Output",
		"",
		mutedStyle.Render("q quit"),
	}, "\n")

	return screenStyle.Render(panelStyle.Render(body))
}

func clamp(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
