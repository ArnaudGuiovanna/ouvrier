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
		title.Render("Ouvrier operate"),
		muted.Render("SOTA local agentic harness for worker operate -> review -> audit -> build -> transfer"),
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

	if len(m.candidates) > 0 && m.session == nil {
		lines = append(lines,
			label.Render("workers"),
			muted.Render("Select an existing worker with 1-9, or create one with `ouvrier operate create-worker --yes ...`."),
		)
		for i, candidate := range m.candidates {
			lines = append(lines, value.Render(fmt.Sprintf("%d  %s  %s", i+1, candidate.Name, candidate.Dir)))
		}
	} else if m.err != nil {
		lines = append(lines,
			errStyle.Render("workspace error: "+m.err.Error()),
			muted.Render("Run from an Ouvrier worker, select one from a parent directory, or create one with `ouvrier operate create-worker`."),
		)
	} else {
		lines = append(lines, workspaceLines(m.workspace, label, value, muted)...)
	}

	lines = append(lines,
		"",
		label.Render("cockpit"),
		value.Render("review worker code, convert findings to AI fixes, audit gates, build artifact, transfer via deploy"),
		"",
		label.Render("available now"),
		value.Render("`ouvrier operate patch|fix-worker|review-worker|audit|build|transfer`"),
		"",
		label.Render("keys"),
		muted.Render("p patch | r review | f fix | a audit | b build | t transfer | q/esc/ctrl+c quit"),
	)
	if len(m.log) > 0 {
		lines = append(lines, "", label.Render("event stream"))
		for _, entry := range tail(m.log, 8) {
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

func compactLines(entries ...string) []string {
	var lines []string
	for _, entry := range entries {
		for _, line := range strings.Split(entry, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
}
