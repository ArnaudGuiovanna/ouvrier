package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
)

// AgentChoice is one locally discovered coding-agent runtime offered during
// cockpit onboarding. It contains readiness metadata only, never credentials.
type AgentChoice struct {
	ID        string
	Label     string
	Transport string
	Auth      string
	Ready     bool
	Detail    string
}

var ErrNoReadyAgent = errors.New("no ready coding agent")

type agentPickerModel struct {
	choices   []AgentChoice
	index     int
	width     int
	height    int
	selected  string
	cancelled bool
}

func newAgentPickerModel(choices []AgentChoice) *agentPickerModel {
	m := &agentPickerModel{
		choices: append([]AgentChoice(nil), choices...),
		index:   -1,
		width:   80,
		height:  24,
	}
	for i := range m.choices {
		if m.choices[i].Ready {
			m.index = i
			break
		}
	}
	return m
}

func (m *agentPickerModel) Init() tea.Cmd { return nil }

func (m *agentPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			m.cancelled = true
			return m, tea.Quit
		case "up", "k":
			m.move(-1)
		case "down", "j", "tab":
			m.move(1)
		case "enter":
			if m.index >= 0 && m.index < len(m.choices) && m.choices[m.index].Ready {
				m.selected = m.choices[m.index].ID
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m *agentPickerModel) move(delta int) {
	if len(m.choices) == 0 || m.index < 0 {
		return
	}
	for step := 1; step <= len(m.choices); step++ {
		next := (m.index + delta*step) % len(m.choices)
		if next < 0 {
			next += len(m.choices)
		}
		if m.choices[next].Ready {
			m.index = next
			return
		}
	}
}

func (m *agentPickerModel) View() tea.View {
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color(accentHex)).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	value := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
	ready := lipgloss.NewStyle().Foreground(lipgloss.Color(okHex))
	unavailable := lipgloss.NewStyle().Foreground(lipgloss.Color(yellowHex))
	selected := lipgloss.NewStyle().
		Foreground(lipgloss.Color(blackHex)).
		Background(lipgloss.Color(accentHex)).
		Bold(true)

	lines := []string{
		accent.Render("Ouvrier Agent Cockpit"),
		value.Render("Choose your coding agent"),
		muted.Render("Ouvrier reuses the local session and stores no credentials."),
		"",
	}
	for i, choice := range m.choices {
		label := strings.TrimSpace(choice.Label)
		if label == "" {
			label = choice.ID
		}
		transport := "ACP v1"
		if strings.TrimSpace(choice.Transport) != "" && choice.Transport != "acp/v1" {
			transport = choice.Transport
		}
		state := ready.Render("ready")
		if !choice.Ready {
			state = unavailable.Render("unavailable")
		}
		row := fmt.Sprintf("  %-14s  %-11s  %s", label, state, muted.Render(transport))
		if i == m.index {
			row = selected.Render("› "+label) + "  " + state + "  " + muted.Render(transport)
		}
		lines = append(lines, row)
		if !choice.Ready && strings.TrimSpace(choice.Detail) != "" {
			lines = append(lines, muted.Render("    "+choice.Detail))
		}
	}
	lines = append(lines, "", muted.Render("↑↓ choose  ·  enter continue  ·  ctrl+c quit"))

	boxWidth := min(max(m.width-6, 44), 76)
	content := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(dimGreenHex)).
		Padding(1, 3).
		Width(boxWidth).
		Render(strings.Join(lines, "\n"))
	view := tea.NewView(content)
	view.AltScreen = true
	view.BackgroundColor = backgroundColor
	view.ForegroundColor = foregroundColor
	view.WindowTitle = "ouvrier · choose agent"
	return view
}

// RunAgentPicker opens the startup ACP agent chooser and returns the selected
// runtime id. It does not initiate or store provider authentication.
func RunAgentPicker(ctx context.Context, in io.Reader, out io.Writer, choices []AgentChoice) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	m := newAgentPickerModel(choices)
	if m.index < 0 {
		return "", ErrNoReadyAgent
	}
	program := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx))
	result, err := program.Run()
	if err != nil {
		return "", err
	}
	final, ok := result.(*agentPickerModel)
	if !ok || final.cancelled || strings.TrimSpace(final.selected) == "" {
		return "", context.Canceled
	}
	return final.selected, nil
}
