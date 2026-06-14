package ide

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate/snippets"
)

// View satisfies the tea.Model interface and returns a tea.View with
// alt-screen, background colour, and window title set.
func (m *ideModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.BackgroundColor = baseRGBA
	v.WindowTitle = "ouvrier ide"
	return v
}

// render composes the full IDE layout as a string.
func (m *ideModel) render() string {
	if !m.ready {
		return "initialising…"
	}

	treeW := 22
	problemsH := 6
	footerH := 2 // status + hints

	// API panel width (only when visible).
	apiW := 0
	if m.showAPI {
		apiW = 28
	}

	// Heights for vertical layout.
	editorH := max(m.height-problemsH-footerH-2, 4)

	left := m.renderTree(treeW, editorH)
	edW := max(m.width-treeW-1-apiW, 20)
	center := m.renderEditor(edW, editorH)

	// Zip left and center panels side by side (with optional API panel).
	leftLines := splitLines(left)
	centerLines := splitLines(center)

	var apiLines []string
	if m.showAPI {
		apiLines = splitLines(m.renderAPIPanel(apiW, editorH))
	}

	totalRows := max(len(leftLines), len(centerLines))
	if m.showAPI {
		totalRows = max(totalRows, len(apiLines))
	}
	for len(leftLines) < totalRows {
		leftLines = append(leftLines, strings.Repeat(" ", treeW))
	}
	for len(centerLines) < totalRows {
		centerLines = append(centerLines, "")
	}
	if m.showAPI {
		for len(apiLines) < totalRows {
			apiLines = append(apiLines, strings.Repeat(" ", apiW))
		}
	}

	var bodyRows []string
	div := lipgloss.NewStyle().Foreground(lipgloss.Color(surface1Hex)).Render("│")
	for i := 0; i < totalRows; i++ {
		row := padRight(leftLines[i], treeW) + div + centerLines[i]
		if m.showAPI {
			row += div + apiLines[i]
		}
		bodyRows = append(bodyRows, row)
	}
	body := strings.Join(bodyRows, "\n")

	problems := m.renderProblems(m.width, problemsH)
	statusLine := m.renderStatusLine(m.width)
	hints := m.renderHints(m.width)

	divider := lipgloss.NewStyle().Foreground(lipgloss.Color(surface1Hex)).Render(strings.Repeat("─", max(m.width, 1)))

	frame := strings.Join([]string{
		body,
		divider,
		problems,
		statusLine,
		hints,
	}, "\n")

	// Overlay the snippet palette if open.
	if m.showPalette {
		frame = m.overlayPalette(frame)
	}

	return frame
}

// renderTree renders the file tree left panel.
func (m *ideModel) renderTree(width, height int) string {
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color(accentHex)).
		Bold(true).
		Width(width).
		Render("worker/ ▾")

	var rows []string
	rows = append(rows, header)

	for i, item := range m.tree {
		indent := strings.Repeat("  ", item.Depth)
		name := item.Rel
		// Use just the base name but keep indent.
		parts := strings.Split(item.Rel, "/")
		if len(parts) > 0 {
			name = parts[len(parts)-1]
		}

		// Check if this file has any errors.
		hasErr := m.fileHasError(item.Rel)

		prefix := "  "
		if item.IsDir {
			prefix = "  "
			name = name + "/"
		}

		style := lipgloss.NewStyle().Width(width)
		if i == m.treeSel && m.focus == regionTree {
			style = style.Background(lipgloss.Color(surface1Hex)).Foreground(lipgloss.Color(textHex))
		} else {
			style = style.Foreground(lipgloss.Color(textHex))
		}
		if item.IsDir {
			style = style.Foreground(lipgloss.Color(accentHex))
		}

		line := indent + prefix + name
		if hasErr {
			errDot := lipgloss.NewStyle().Foreground(lipgloss.Color(diagErrorHex)).Render("● ")
			line = indent + errDot + name
		}
		rows = append(rows, style.Render(line))
	}

	// Pad to height.
	for len(rows) < height {
		rows = append(rows, strings.Repeat(" ", width))
	}
	return strings.Join(rows[:min(len(rows), height)], "\n")
}

// renderEditor renders the editor center panel with a header line.
func (m *ideModel) renderEditor(width, height int) string {
	dirtyMark := ""
	if m.dirty {
		dirtyMark = lipgloss.NewStyle().Foreground(lipgloss.Color(gateHex)).Render(" ●modified")
	}
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color(subtext0Hex)).
		Width(width).
		Render(m.openPath + dirtyMark)

	editorView := m.editor.View()
	return header + "\n" + editorView
}

// renderProblems renders the problems panel.
func (m *ideModel) renderProblems(width, height int) string {
	n := len(m.problems)
	title := fmt.Sprintf("Problems (%d)", n)
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color(accentHex)).
		Bold(true).
		Render(title)

	var rows []string
	rows = append(rows, header)

	if n == 0 {
		ok := lipgloss.NewStyle().Foreground(lipgloss.Color(okHex)).Render("✓ no problems")
		rows = append(rows, ok)
	} else {
		for i, p := range m.problems {
			glyph := severityGlyph(p.Severity)
			col := severityColor(p.Severity)
			sevStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(col))

			loc := p.File
			if p.Line > 0 {
				loc = fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
			}
			origin := ""
			if p.Origin != "" {
				origin = "  (" + p.Origin + ")"
			}
			line := fmt.Sprintf("%s %s  %s%s", sevStyle.Render(glyph), loc, p.Message, origin)
			if i == m.problemSel && m.focus == regionProblems {
				line = lipgloss.NewStyle().
					Background(lipgloss.Color(surface1Hex)).
					Foreground(lipgloss.Color(textHex)).
					Width(width).
					Render(line)
			}
			rows = append(rows, line)
			if len(rows) >= height {
				break
			}
		}
	}
	// Pad to height.
	for len(rows) < height {
		rows = append(rows, "")
	}
	return strings.Join(rows[:min(len(rows), height)], "\n")
}

// renderStatusLine renders the single-line status bar.
func (m *ideModel) renderStatusLine(width int) string {
	statusColor := overlay2Hex
	switch m.statusKind {
	case "ok":
		statusColor = okHex
	case "running":
		statusColor = runningHex
	case "fail":
		statusColor = failHex
	}
	statusPart := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor)).Render(m.status)
	lspPart := lipgloss.NewStyle().Foreground(lipgloss.Color(overlay1Hex)).Render(m.lspStatus)

	bar := lipgloss.NewStyle().
		Foreground(lipgloss.Color(subtext0Hex)).
		Width(width).
		Background(lipgloss.Color(mantleHex)).
		Render(fmt.Sprintf(" REVIEW · %s · %s · %s", m.openPath, statusPart, lspPart))
	return bar
}

// renderHints renders the footer keybinding hints line.
func (m *ideModel) renderHints(width int) string {
	hints := "ctrl+s save&audit · ctrl+b build · tab focus · ]d/[d problem · ctrl+p snippets · ctrl+\\ API · ctrl+q quit"
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(overlay1Hex)).
		Width(width).
		Render(hints)
}

// renderAPIPanel renders the right-docked Ouvrier API reference panel.
func (m *ideModel) renderAPIPanel(width, height int) string {
	header := lipgloss.NewStyle().
		Foreground(lipgloss.Color(accentHex)).
		Bold(true).
		Width(width).
		Render("Ouvrier API ◇")

	all := snippets.All()
	var rows []string
	rows = append(rows, header)

	currentGroup := ""
	groupStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(overlay2Hex)).Bold(true).Width(width)
	rowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(accentHex)).Width(width)
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color(surface1Hex)).Foreground(lipgloss.Color(textHex)).Width(width)

	for i, s := range all {
		if s.Group != currentGroup {
			currentGroup = s.Group
			rows = append(rows, groupStyle.Render("── "+currentGroup))
		}
		label := "◇ " + s.Prefix + " — " + s.Title
		if i == m.apiSel {
			rows = append(rows, selStyle.Render(label))
		} else {
			rows = append(rows, rowStyle.Render(label))
		}
		if len(rows) >= height-1 {
			break
		}
	}

	// Pad to height.
	for len(rows) < height {
		rows = append(rows, strings.Repeat(" ", width))
	}
	return strings.Join(rows[:min(len(rows), height)], "\n")
}

// overlayPalette renders the snippet palette as a centered block overlaid on
// the frame string by replacing the center rows of text.
func (m *ideModel) overlayPalette(frame string) string {
	const paletteW = 60
	const maxRows = 10 // header + query + 8 items

	// Build palette content lines.
	var lines []string

	titleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(accentHex)).Bold(true)
	queryStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(textHex))
	itemStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(textHex))
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color(accentHex)).Foreground(lipgloss.Color(baseHex))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(overlay2Hex))

	lines = append(lines, titleStyle.Render("  Ouvrier snippets"))
	lines = append(lines, queryStyle.Render("  > "+m.paletteQuery+"█"))

	visible := m.paletteItems
	if len(visible) > 8 {
		visible = visible[:8]
	}
	if len(visible) == 0 {
		lines = append(lines, itemStyle.Render("  (no matches)"))
	}
	for i, s := range visible {
		label := "◇ " + s.Prefix + "  " + s.Title
		if i == m.paletteSel {
			lines = append(lines, selStyle.Width(paletteW-2).Render("  "+label))
		} else {
			lines = append(lines, itemStyle.Render("  "+label))
		}
	}
	lines = append(lines, hintStyle.Render("  enter insert · esc close"))

	// Box the palette.
	boxTop := lipgloss.NewStyle().Foreground(lipgloss.Color(surface2Hex)).Render("┌" + strings.Repeat("─", paletteW-2) + "┐")
	boxBot := lipgloss.NewStyle().Foreground(lipgloss.Color(surface2Hex)).Render("└" + strings.Repeat("─", paletteW-2) + "┘")
	borderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(surface2Hex))
	_ = borderStyle

	boxLines := []string{boxTop}
	for _, l := range lines {
		padded := padRight(l, paletteW-2)
		boxLines = append(boxLines, "│"+padded+"│")
	}
	boxLines = append(boxLines, boxBot)

	// Overlay: replace rows starting at row 2 (after tree header) centered.
	frameLines := strings.Split(frame, "\n")
	startRow := 2
	if m.height > maxRows+4 {
		startRow = (m.height - len(boxLines)) / 4
		if startRow < 1 {
			startRow = 1
		}
	}

	startCol := (m.width - paletteW) / 2
	if startCol < 0 {
		startCol = 0
	}
	prefix := strings.Repeat(" ", startCol)

	for i, bl := range boxLines {
		row := startRow + i
		if row >= len(frameLines) {
			break
		}
		frameLines[row] = prefix + bl
	}
	return strings.Join(frameLines, "\n")
}

// fileHasError returns true if any diagnostic error exists for the given
// workspace-relative path.
func (m *ideModel) fileHasError(rel string) bool {
	for _, p := range m.problems {
		if p.File == rel && p.Severity == 1 {
			return true
		}
	}
	return false
}

// splitLines splits a string into lines.
func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

// padRight right-pads s with spaces to length n.
func padRight(s string, n int) string {
	// Strip ANSI then measure? Just use a width-bounded lipgloss render.
	// Use lipgloss to get a width-bounded string.
	return lipgloss.NewStyle().Width(n).Render(s)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
