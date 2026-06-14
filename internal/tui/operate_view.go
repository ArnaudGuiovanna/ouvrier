package tui

import (
	"fmt"
	"strings"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func renderOperate(m *operateModel) string {
	termWidth := clamp(m.width, 56, 160)
	mainWidth := termWidth - 8
	if mainWidth < 48 {
		mainWidth = 48
	}

	title := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex)).Faint(true)
	label := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	value := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5f5f")).Bold(true)
	panel := lipgloss.NewStyle().
		Foreground(lipgloss.Color(offWhiteHex)).
		Background(lipgloss.Color(blackHex)).
		Border(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color(greenHex)).
		Padding(1, 2).
		Width(mainWidth)
	screen := lipgloss.NewStyle().
		Foreground(lipgloss.Color(offWhiteHex)).
		Background(lipgloss.Color(blackHex)).
		Padding(1, 2)

	lines := []string{
		title.Render("Ouvrier Agent Cockpit"),
		muted.Render("Prompt-first worker factory: operate -> review -> audit -> build -> transfer"),
		"",
		label.Render("mode") + "  " + value.Render(m.mode),
		label.Render("agent") + " " + value.Render(agentLine(m.opts)),
	}
	if m.session != nil {
		lines = append(lines, label.Render("session")+" "+value.Render(m.session.ID+" / "+string(m.session.Status)))
	} else if m.opts.Session != "" {
		lines = append(lines, label.Render("session")+" "+value.Render(m.opts.Session))
	}
	if m.running != "" {
		lines = append(lines, label.Render("running")+" "+value.Render(m.running))
	}
	if m.opts.Goal != "" {
		lines = append(lines, label.Render("goal")+" "+value.Render(m.opts.Goal))
	}
	if m.opts.Env != "" {
		lines = append(lines, label.Render("env")+"   "+value.Render(m.opts.Env))
	}
	lines = append(lines, "")

	if len(m.candidates) > 0 && m.mode == "select" {
		lines = append(lines,
			label.Render("workers"),
			muted.Render("Select an existing worker with 1-9, or type a prompt to create a new one."),
		)
		for i, candidate := range m.candidates {
			lines = append(lines, value.Render(fmt.Sprintf("%d  %s  %s", i+1, candidate.Name, candidate.Dir)))
		}
	} else if m.err != nil {
		lines = append(lines,
			errStyle.Render("workspace error: "+m.err.Error()),
			muted.Render("Run from an Ouvrier worker, select one from a parent directory, or create one with `ouvrier operate create-worker`."),
		)
	} else if m.workspace.Dir == "" {
		lines = append(lines,
			label.Render("factory")+" "+value.Render(m.opts.Dir),
			muted.Render("No worker selected yet. Type a prompt such as: create a worker that receives POST /tickets."),
		)
	} else {
		lines = append(lines, workspaceLines(m.workspace, label, value, muted)...)
	}

	lines = append(lines,
		"",
		label.Render("transcript"),
	)
	if len(m.transcript) == 0 {
		lines = append(lines, muted.Render("No turns yet. Type a worker goal or /help."))
	} else {
		lines = append(lines, transcriptLines(m.transcript, label, value, muted)...)
	}
	lines = append(lines,
		"",
		label.Render("prompt"),
		value.Render(m.input.View()),
		muted.Render("/login codex | /new worker | /read | /review | /fix | /audit | /build | /deploy | /accept-risk | /export | /help"),
	)
	if len(m.log) > 0 {
		lines = append(lines, "", label.Render("event stream"))
		for _, entry := range tail(m.log, 5) {
			lines = append(lines, muted.Render(entry))
		}
	}

	return screen.Render(panel.Render(joinWrapped(lines)))
}

func workspaceLines(ws operate.Workspace, label, value, muted lipgloss.Style) []string {
	lines := []string{
		label.Render("worker") + " " + value.Render(ws.Name),
		label.Render("dir") + "    " + value.Render(ws.Dir),
		label.Render("git") + "    " + value.Render(gitLine(ws.Git)),
	}
	if len(ws.DeployEnvs) > 0 {
		lines = append(lines, label.Render("deploy")+" "+value.Render(strings.Join(ws.DeployEnvs, ", ")))
	} else {
		lines = append(lines, label.Render("deploy")+" "+muted.Render("no deploy environments with hosts in pip.yaml"))
	}
	if len(ws.Events) > 0 {
		lines = append(lines, label.Render("events")+" "+value.Render(strings.Join(ws.Events, ", ")))
	}
	if len(ws.Outcomes) > 0 {
		lines = append(lines, label.Render("outcomes")+" "+value.Render(strings.Join(ws.Outcomes, ", ")))
	}
	return lines
}

func transcriptLines(entries []operate.TranscriptEntry, label, value, muted lipgloss.Style) []string {
	var lines []string
	for _, entry := range tailTranscript(entries, 12) {
		switch entry.Kind {
		case operate.TranscriptUser:
			lines = append(lines, label.Render("you")+" "+value.Render(entry.Text))
		case operate.TranscriptAssistant:
			lines = append(lines, label.Render("agent")+" "+value.Render(compactTranscriptText(entry.Text)))
		case operate.TranscriptToolCall:
			lines = append(lines, muted.Render("tool "+entry.ToolName+" started"))
		case operate.TranscriptToolResult:
			summary, _ := entry.Output["summary"].(string)
			if summary == "" {
				summary = "done"
			}
			lines = append(lines, muted.Render("tool "+entry.ToolName+" -> "+compactTranscriptText(summary)))
		case operate.TranscriptError:
			lines = append(lines, muted.Render("error "+compactTranscriptText(entry.Text)))
		case operate.TranscriptStatus:
			lines = append(lines, muted.Render(compactTranscriptText(entry.Text)))
		}
	}
	return lines
}

func agentLine(opts OperateOptions) string {
	if opts.Agent == "" {
		return "codex"
	}
	if opts.Agent == "codex" {
		return fmt.Sprintf("codex (%s)", opts.CodexMode)
	}
	return opts.Agent
}

func gitLine(info operate.GitInfo) string {
	if !info.Present {
		return "not a Git worktree"
	}
	branch := info.Branch
	if branch == "" {
		branch = "detached"
	}
	if info.Dirty {
		return branch + " dirty"
	}
	return branch + " clean"
}

func joinWrapped(lines []string) string {
	return strings.Join(lines, "\n")
}

func appendBounded(log []string, entries ...string) []string {
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		log = append(log, entry)
	}
	if len(log) > 64 {
		return log[len(log)-64:]
	}
	return log
}

func tail(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

func tailTranscript(entries []operate.TranscriptEntry, n int) []operate.TranscriptEntry {
	if len(entries) <= n {
		return entries
	}
	return entries[len(entries)-n:]
}

func compactTranscriptText(text string) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if len(text) > 180 {
		return text[:177] + "..."
	}
	return text
}
