package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

// render composes the full cockpit frame: scrolling transcript, optional slash
// menu, multiline composer, and a status bar — the four Pi-style regions.
func (m *operateModel) render() string {
	if !m.ready {
		return ""
	}
	if m.showHelp {
		return m.renderHelp()
	}

	cw := m.vp.Width()
	sections := []string{
		m.vp.View(),
	}
	if m.slashActive && len(m.slashMatches) > 0 {
		sections = append(sections, m.renderSlashMenu(cw))
	}
	if m.pendingApproval != nil {
		sections = append(sections, m.renderApprovalCard(cw))
	}
	sections = append(sections,
		m.renderRule(cw),
		m.composer.View(),
		m.renderStatusBar(cw),
		m.renderHints(cw),
	)
	return strings.Join(sections, "\n")
}

func (m *operateModel) renderRule(width int) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(dimGreenHex)).Render(strings.Repeat("─", max(width, 1)))
}

// renderTranscript renders every block into the viewport content area.
func (m *operateModel) renderTranscript() string {
	width := m.vp.Width()
	if width <= 0 {
		width = m.width - 2
	}
	var out []string
	if !m.hasUserTurn() {
		out = append(out, m.renderWelcome(width), "")
	}
	if m.mode == "select" && len(m.candidates) > 0 {
		out = append(out, m.renderCandidates(width), "")
	}
	if m.err != nil {
		out = append(out, renderBlockError(width, "workspace error: "+m.err.Error()), "")
	}
	for i, b := range m.blocks {
		out = append(out, m.renderBlock(width, b))
		if i < len(m.blocks)-1 {
			out = append(out, "")
		}
	}
	return strings.Join(out, "\n")
}

func (m *operateModel) hasUserTurn() bool {
	for _, b := range m.blocks {
		if b.kind == blockUser {
			return true
		}
	}
	return false
}

func (m *operateModel) renderWelcome(width int) string {
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	cyan := lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex))
	lines := []string{
		accent.Render("◢ Ouvrier Agent Cockpit"),
		muted.Render("The terminal worker factory — prompt → plan → build → review → deploy."),
		"",
		muted.Render("Try: ") + cyan.Render("create a worker that receives POST /tickets and triages it"),
		muted.Render("Or:  ") + cyan.Render("/review") + muted.Render("  ") + cyan.Render("/audit") + muted.Render("  ") + cyan.Render("/build linux/amd64") + muted.Render("  ") + cyan.Render("/deploy staging"),
		muted.Render("Type ") + cyan.Render("/") + muted.Render(" for commands, ") + cyan.Render("?") + muted.Render(" for help."),
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(dimGreenHex)).
		Padding(0, 2).
		Width(max(width-2, 20))
	return box.Render(strings.Join(lines, "\n"))
}

func (m *operateModel) renderCandidates(width int) string {
	label := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	value := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
	lines := []string{
		label.Render("Detected workers"),
		muted.Render("Press 1-9 to open one, or type a prompt to create a new worker."),
	}
	for i, c := range m.candidates {
		lines = append(lines, fmt.Sprintf("%s  %s  %s",
			label.Render(fmt.Sprintf("%d", i+1)),
			value.Render(c.Name),
			muted.Render(c.Dir),
		))
	}
	return strings.Join(lines, "\n")
}

func (m *operateModel) renderBlock(width int, b opBlock) string {
	switch b.kind {
	case blockUser:
		return renderBlockUser(width, b.text)
	case blockAssistant:
		return m.renderBlockAssistant(width, b)
	case blockTool:
		return m.renderBlockTool(width, b)
	case blockNotice:
		return renderBlockNotice(width, b.text)
	case blockError:
		return renderBlockError(width, b.text)
	}
	return ""
}

func renderBlockUser(width int, text string) string {
	icon := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true).Render("❯")
	body := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex)).Bold(true).Render(wrapText(text, width-2))
	return indentBlock(icon+" ", "  ", body)
}

func (m *operateModel) renderBlockAssistant(width int, b opBlock) string {
	dot := lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex)).Render("●")
	text := b.text
	if b.streaming {
		text += lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex)).Render("▌")
	}
	body := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex)).Render(wrapText(text, width-2))
	return indentBlock(dot+" ", "  ", body)
}

func (m *operateModel) renderBlockTool(width int, b opBlock) string {
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(dimGreenHex))
	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))

	var badge string
	switch {
	case b.running:
		badge = m.spin.View() + " " + keyStyle.Render("running")
	case b.toolErr:
		badge = lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Bold(true).Render("✗ failed")
	default:
		badge = lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Render("✓")
	}

	header := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Render("⚙ ") + nameStyle.Render(b.toolName) + "  " + badge

	var lines []string
	lines = append(lines, header)
	for _, kv := range formatToolInput(b.toolInput) {
		lines = append(lines, keyStyle.Render(kv.k+": ")+valStyle.Render(kv.v))
	}
	if !b.running {
		summary := toolSummary(b.toolOutput)
		if b.toolErr {
			if e := stringFromMap(b.toolOutput, "error"); e != "" {
				summary = e
			}
			lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Render("→ "+summary))
		} else if summary != "" {
			lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex)).Render("→ ")+valStyle.Render(summary))
		}
		for _, extra := range toolExtraLines(b.toolOutput) {
			lines = append(lines, keyStyle.Render("  "+extra))
		}
	}

	prefix := bar.Render("│ ")
	wrapped := make([]string, 0, len(lines))
	for _, ln := range lines {
		for _, sub := range strings.Split(wrapText(ln, width-3), "\n") {
			wrapped = append(wrapped, prefix+sub)
		}
	}
	return strings.Join(wrapped, "\n")
}

func renderBlockNotice(width int, text string) string {
	dot := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex)).Render("·")
	body := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex)).Render(wrapText(text, width-2))
	return indentBlock(dot+" ", "  ", body)
}

func renderBlockError(width int, text string) string {
	icon := lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Bold(true).Render("✗")
	body := lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Render(wrapText(text, width-2))
	return indentBlock(icon+" ", "  ", body)
}

func (m *operateModel) renderSlashMenu(width int) string {
	selStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(blackHex)).Background(lipgloss.Color(greenHex)).Bold(true)
	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex))
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	var lines []string
	n := min(len(m.slashMatches), 6)
	for i := 0; i < n; i++ {
		cmd := m.slashMatches[i]
		name := fmt.Sprintf(" %-12s ", cmd.name)
		row := nameStyle.Render(name) + descStyle.Render(cmd.desc)
		if i == m.slashIndex {
			row = selStyle.Render(name) + " " + descStyle.Render(cmd.desc)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

func (m *operateModel) renderStatusBar(width int) string {
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color(dimGreenHex)).Render(" · ")
	seg := func(label, value string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex)).Render(label+" ") +
			lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex)).Render(value)
	}

	var state string
	switch {
	case m.running:
		elapsed := time.Since(m.startedAt).Round(time.Second)
		state = lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Render(m.spin.View() + " working " + elapsed.String())
	case m.status != "":
		state = lipgloss.NewStyle().Foreground(lipgloss.Color(yellowHex)).Render(m.status)
	default:
		state = lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Render("ready")
	}

	worker := m.workspace.Name
	if worker == "" {
		worker = shortPath(m.opts.Dir)
	}
	segs := []string{
		state,
		seg("model", m.currentModel()),
		seg("worker", worker),
		seg("posture", string(m.posture)),
	}
	if m.workspace.Git.Present {
		segs = append(segs, seg("git", gitLine(m.workspace.Git)))
	}
	if m.session != nil {
		segs = append(segs, seg("session", shortID(m.session.ID)))
	}
	if len(m.queue) > 0 {
		segs = append(segs, lipgloss.NewStyle().Foreground(lipgloss.Color(yellowHex)).Render(fmt.Sprintf("queued %d", len(m.queue))))
	}
	line := strings.Join(segs, sep)
	return lipgloss.NewStyle().Width(width).Render(truncateANSI(line, width))
}

func (m *operateModel) renderHints(width int) string {
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	key := lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex))
	parts := []string{
		key.Render("enter") + hint.Render(" send"),
		key.Render("alt+enter") + hint.Render(" newline"),
		key.Render("esc") + hint.Render(" stop"),
		key.Render("ctrl+p") + hint.Render(" model"),
		key.Render("/") + hint.Render(" cmds"),
		key.Render("?") + hint.Render(" help"),
		key.Render("ctrl+c") + hint.Render(" quit"),
	}
	if m.running {
		parts = []string{
			key.Render("esc") + hint.Render(" interrupt"),
			key.Render("enter") + hint.Render(" queue follow-up"),
			key.Render("pgup/pgdn") + hint.Render(" scroll"),
			key.Render("ctrl+c") + hint.Render(" quit"),
		}
	}
	return truncateANSI(strings.Join(parts, hint.Render("  ")), width)
}

func (m *operateModel) renderHelp() string {
	title := lipgloss.NewStyle().Foreground(lipgloss.Color(greenHex)).Bold(true)
	key := lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	val := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))

	lines := []string{
		title.Render("Ouvrier Agent Cockpit — help"),
		"",
		title.Render("Keys"),
		key.Render("  enter") + muted.Render("       send prompt / run selected command"),
		key.Render("  alt+enter") + muted.Render("   insert newline (multiline prompt)"),
		key.Render("  esc") + muted.Render("         interrupt the running turn / close menu"),
		key.Render("  ctrl+p") + muted.Render("      cycle model"),
		key.Render("  pgup/pgdn") + muted.Render("   scroll transcript"),
		key.Render("  /") + muted.Render("           open the command menu"),
		key.Render("  ?") + muted.Render("           toggle this help"),
		key.Render("  ctrl+c") + muted.Render("      quit (session is saved)"),
		"",
		title.Render("Commands"),
	}
	cmds := append([]slashCmd(nil), operateSlashCommands...)
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].name < cmds[j].name })
	for _, c := range cmds {
		lines = append(lines, "  "+key.Render(fmt.Sprintf("%-12s", c.name))+" "+val.Render(c.usage))
	}
	lines = append(lines, "", muted.Render("Natural language covers the same actions — slash commands are accelerators."))
	lines = append(lines, "", muted.Render("Press any key to return."))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(greenHex)).
		Padding(1, 3).
		Width(max(m.width-4, 40))
	return box.Render(strings.Join(lines, "\n"))
}

// --- tool card formatting helpers ---

type kv struct{ k, v string }

func formatToolInput(input map[string]any) []kv {
	if len(input) == 0 {
		return nil
	}
	keys := make([]string, 0, len(input))
	for k := range input {
		if k == "summary" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []kv
	for _, k := range keys {
		v := compactValue(input[k])
		if v == "" {
			continue
		}
		out = append(out, kv{k: k, v: v})
		if len(out) == 4 {
			break
		}
	}
	return out
}

func toolSummary(output map[string]any) string {
	if s := stringFromMap(output, "summary"); s != "" {
		return s
	}
	return "done"
}

func toolExtraLines(output map[string]any) []string {
	if output == nil {
		return nil
	}
	var out []string
	if files, ok := output["changed_files"].([]any); ok {
		for _, f := range files {
			if s, ok := f.(string); ok {
				out = append(out, s)
			}
			if len(out) == 5 {
				break
			}
		}
	}
	return out
}

func compactValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.Join(strings.Fields(t), " ")
	case bool:
		return fmt.Sprintf("%t", t)
	case float64:
		return fmt.Sprintf("%g", t)
	case int:
		return fmt.Sprintf("%d", t)
	case nil:
		return ""
	default:
		s := fmt.Sprintf("%v", t)
		if len(s) > 80 {
			return s[:77] + "…"
		}
		return s
	}
}

func stringFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.Join(strings.Fields(s), " ")
	}
	return ""
}

// --- low-level text helpers ---

func indentBlock(firstPrefix, contPrefix, body string) string {
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		if i == 0 {
			lines[i] = firstPrefix + ln
		} else {
			lines[i] = contPrefix + ln
		}
	}
	return strings.Join(lines, "\n")
}

// wrapText wraps text to the given width, preserving existing newlines. It is
// width-aware on rune count (good enough for the cockpit's ASCII content).
func wrapText(text string, width int) string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, paragraph := range strings.Split(text, "\n") {
		out = append(out, wrapParagraph(paragraph, width))
	}
	return strings.Join(out, "\n")
}

func wrapParagraph(text string, width int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len([]rune(cur))+1+len([]rune(w)) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	lines = append(lines, cur)
	return strings.Join(lines, "\n")
}

func gitLine(info operate.GitInfo) string {
	if !info.Present {
		return "no git"
	}
	branch := info.Branch
	if branch == "" {
		branch = "detached"
	}
	if info.Dirty {
		return branch + "*"
	}
	return branch
}

func shortPath(p string) string {
	if p == "" || p == "." {
		return "."
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncateANSI(s string, width int) string {
	if width <= 0 {
		return s
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	// Fall back to rune truncation on the visible content.
	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}

func (m *operateModel) renderApprovalCard(width int) string {
	ap := m.pendingApproval
	if ap == nil {
		return ""
	}
	title := lipgloss.NewStyle().Foreground(lipgloss.Color(magentaHex)).Bold(true)
	key := lipgloss.NewStyle().Foreground(lipgloss.Color(cyanHex))
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(mutedHex))
	val := lipgloss.NewStyle().Foreground(lipgloss.Color(offWhiteHex))
	var lines []string
	lines = append(lines, title.Render("◆ Approval required"))
	lines = append(lines, val.Render(ap.Summary))
	for _, kv := range approvalDetailLines(ap) {
		lines = append(lines, muted.Render(kv.k+": ")+val.Render(kv.v))
	}
	if ap.Prod {
		want := workerNameForApproval(ap)
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color(redHex)).Render(
			"PROD — type \""+want+"\" then enter: "+m.prodConfirm))
		lines = append(lines, key.Render("type name + enter")+muted.Render(" approve   ")+key.Render("esc")+muted.Render(" deny"))
	} else {
		lines = append(lines, key.Render("enter/y")+muted.Render(" approve   ")+key.Render("esc/n")+muted.Render(" deny"))
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(magentaHex)).
		Padding(0, 2).
		Width(max(width-2, 20))
	return box.Render(strings.Join(lines, "\n"))
}

func approvalDetailLines(ap *operate.ApprovalRequest) []kv {
	if ap == nil || ap.Details == nil {
		return nil
	}
	keys := make([]string, 0, len(ap.Details))
	for k := range ap.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []kv
	for _, k := range keys {
		v := compactValue(ap.Details[k])
		if v == "" {
			continue
		}
		out = append(out, kv{k: k, v: v})
		if len(out) == 6 {
			break
		}
	}
	return out
}
