package ide

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate/snippets"
)

// region is the keyboard-focus region of the IDE layout.
type region int

const (
	regionTree region = iota
	regionEditor
	regionProblems
)

// IDEOptions configure the Ouvrier IDE TUI.
type IDEOptions struct {
	Workspace operate.Workspace
	GoplsPath string // "" -> no LSP
}

// ideModel is the Bubble Tea model for the IDE.
type ideModel struct {
	ctx    context.Context
	ws     operate.Workspace
	width  int
	height int
	ready  bool

	// file tree
	tree    []treeItem
	treeSel int

	// editor
	openPath string // workspace-relative
	editor   textarea.Model
	dirty    bool

	// LSP
	client    *lsp.Client
	enc       lsp.PositionEncoding
	diags     map[string][]lsp.Diagnostic // key = absolute URI
	lspStatus string

	// problems panel
	problems   []Problem
	problemSel int
	auditProbs []Problem // kept separately so audit problems survive LSP refreshes

	// focus
	focus region

	// status bar
	status      string
	statusKind  string // "ok" | "running" | "fail" | "info"
	building    bool
	auditPassed bool

	// GoplsPath stored for retry
	goplsPath string

	// snippet palette overlay
	showPalette  bool
	paletteQuery string
	paletteSel   int
	paletteItems []snippets.Snippet

	// API reference panel
	showAPI bool
	apiSel  int

	// hover overlay
	showHover bool
	hoverText string

	// completion popup
	showComplete      bool
	completeItems     []completeItem
	completeSel       int
	completeWordStart int // rune col where the current identifier starts
	docVersion        int // incremented on each DidChange after acceptCompletion

	// definition jump stack
	jumpStack []jumpLoc
}

// completeItem is one entry in the completion popup.
type completeItem struct {
	label   string
	detail  string
	insert  string        // text to insert when no TextEdit
	edit    *lsp.TextEdit // gopls authoritative edit (may be nil)
	addl    []lsp.TextEdit
	snippet bool // true for Ouvrier snippets (mauve ◇, sorted first)
}

// completeMsg carries completion results from a gopls call.
type completeMsg struct {
	items []completeItem
}

// jumpLoc records a cursor position for jump-back (ctrl+t).
type jumpLoc struct {
	path string
	row  int
	col  int
}

// --- message types ---

type lspReadyMsg struct {
	client *lsp.Client
	err    error
}

type diagMsg struct {
	params lsp.PublishDiagnosticsParams
}

type auditMsg struct {
	report operate.AuditReport
	err    error
}

type buildMsg struct {
	artifact operate.BuildArtifact
	err      error
}

type hoverMsg struct {
	text string
}

type defMsg struct {
	locs []lsp.Location
}

// --- constructors ---

// RunIDE drives the IDE TUI until the user exits.
func RunIDE(ctx context.Context, in io.Reader, out io.Writer, opts IDEOptions) error {
	program := tea.NewProgram(
		newIDEModel(ctx, opts),
		tea.WithInput(in),
		tea.WithOutput(out),
	)
	_, err := program.Run()
	return err
}

// newIDEModel creates a new ideModel without starting the Bubble Tea program.
// Exported as NewIDEModel for use from tests and callers in this package.
func newIDEModel(ctx context.Context, opts IDEOptions) *ideModel {
	ed := textarea.New()
	ed.ShowLineNumbers = true
	ed.MaxHeight = 0
	ed.Focus()

	m := &ideModel{
		ctx:       ctx,
		ws:        opts.Workspace,
		width:     100,
		height:    32,
		openPath:  "main.go",
		editor:    ed,
		diags:     make(map[string][]lsp.Diagnostic),
		focus:     regionEditor,
		goplsPath: opts.GoplsPath,
		lspStatus: "",
	}

	// Determine the initial open path from the workspace manifest.
	if opts.Workspace.MainPath != "" {
		rel, err := filepath.Rel(opts.Workspace.Dir, opts.Workspace.MainPath)
		if err == nil {
			m.openPath = rel
		}
	}

	if opts.GoplsPath == "" {
		m.lspStatus = "gopls not found — go install golang.org/x/tools/gopls@latest · r retry"
	}

	return m
}

// NewIDEModel is the exported constructor for testing from outside the package.
func NewIDEModel(ctx context.Context, opts IDEOptions) *ideModel {
	return newIDEModel(ctx, opts)
}

// cursorPos returns the current editor cursor as an LSP Position using the
// negotiated encoding.
func (m *ideModel) cursorPos() lsp.Position {
	row := m.editor.Line()
	col := m.editor.Column()
	lineText := lsp.LineAt(m.editor.Value(), row)
	return lsp.Position{Line: row, Character: lsp.EncodedColumn(lineText, col, m.enc)}
}

// --- tea.Model interface ---

// Init sets up the file tree, loads the initial file, and starts gopls if configured.
func (m *ideModel) Init() tea.Cmd {
	// Build initial tree.
	if m.ws.Dir != "" {
		m.tree = buildTree(m.ws.Dir)
	}

	// Load the initial file.
	if m.ws.Dir != "" {
		content, err := operate.ReadWorkerFile(m.ws, m.openPath)
		if err == nil {
			m.editor.SetValue(content)
		}
	}

	var cmds []tea.Cmd

	// Start gopls if configured.
	if m.goplsPath != "" {
		goplsPath := m.goplsPath
		wsDir := m.ws.Dir
		openPath := m.openPath
		editorContent := m.editor.Value()
		cmds = append(cmds, func() tea.Msg {
			// The gopls process must outlive this cmd; lsp.New bounds only the
			// initialize round-trip internally. Passing a defer-cancelled ctx
			// here would kill gopls before it publishes diagnostics.
			root := moduleRoot(wsDir)
			client, err := lsp.New(m.ctx, goplsPath, root)
			if err != nil {
				return lspReadyMsg{err: err}
			}
			// Notify open.
			uri := lsp.URI(filepath.Join(wsDir, openPath))
			_ = client.DidOpen(uri, "go", editorContent, 1)
			return lspReadyMsg{client: client}
		})
	}

	cmds = append(cmds, m.editor.Focus())
	return tea.Batch(cmds...)
}

// Update handles all incoming messages.
func (m *ideModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		edW := max(m.width-24, 20)
		edH := max(m.height-8, 4)
		m.editor.SetWidth(edW)
		m.editor.SetHeight(edH)
		return m, nil

	case lspReadyMsg:
		if msg.err != nil {
			m.lspStatus = "lsp: degraded — r to retry"
			return m, nil
		}
		m.client = msg.client
		m.enc = msg.client.Encoding()
		m.lspStatus = "lsp: ready"
		return m, listenDiag(m.client)

	case diagMsg:
		m.diags[msg.params.URI] = msg.params.Diagnostics
		m.rebuildProblems()
		if m.client == nil {
			return m, nil
		}
		return m, listenDiag(m.client)

	case auditMsg:
		// Convert audit gate results to Problem records.
		m.auditProbs = nil
		if msg.report.Passed {
			m.auditPassed = true
			m.status = "audit passed"
			m.statusKind = "ok"
		} else {
			m.auditPassed = false
			m.status = "audit failed"
			m.statusKind = "fail"
		}
		for _, gr := range msg.report.Results {
			if gr.Status == operate.GateFail {
				p := Problem{
					Source:   "audit",
					File:     "",
					Line:     0,
					Col:      0,
					Severity: 1,
					Message:  gr.Error,
					Origin:   gr.Name,
				}
				m.auditProbs = append(m.auditProbs, p)
			}
		}
		m.rebuildProblems()
		return m, nil

	case buildMsg:
		m.building = false
		if msg.err != nil {
			m.status = "build failed: " + msg.err.Error()
			m.statusKind = "fail"
		} else {
			m.status = "build ok — " + msg.artifact.BinaryPath
			m.statusKind = "ok"
		}
		return m, nil

	case hoverMsg:
		if msg.text != "" {
			m.showHover = true
			m.hoverText = msg.text
		} else {
			m.showHover = false
		}
		return m, nil

	case completeMsg:
		if m.showComplete {
			seen := make(map[string]bool, len(m.completeItems))
			for _, it := range m.completeItems {
				seen[it.label] = true
			}
			for _, it := range msg.items {
				if !seen[it.label] {
					m.completeItems = append(m.completeItems, it)
					seen[it.label] = true
				}
			}
			// Clamp selection.
			if m.completeSel >= len(m.completeItems) {
				m.completeSel = max(0, len(m.completeItems)-1)
			}
		}
		return m, nil

	case defMsg:
		if len(msg.locs) == 0 {
			return m, nil
		}
		loc := msg.locs[0]
		absPath := uriToPath(loc.URI)
		var rel string
		if m.ws.Dir != "" {
			if r, err := filepath.Rel(m.ws.Dir, absPath); err == nil && !filepath.IsAbs(r) {
				rel = r
			}
		}
		if rel == "" {
			m.status = "definition is outside the worker (" + absPath + ")"
			m.statusKind = "info"
			return m, nil
		}
		// Push current position onto jump stack.
		m.jumpStack = append(m.jumpStack, jumpLoc{
			path: m.openPath,
			row:  m.editor.Line(),
			col:  m.editor.Column(),
		})
		// Open the target file.
		content, err := operate.ReadWorkerFile(m.ws, rel)
		if err != nil {
			m.status = "definition: open " + rel + ": " + err.Error()
			m.statusKind = "fail"
			return m, nil
		}
		m.openPath = rel
		m.editor.SetValue(content)
		m.dirty = false
		if m.client != nil {
			uri := lsp.URI(filepath.Join(m.ws.Dir, rel))
			_ = m.client.DidOpen(uri, "go", content, 1)
		}
		targetLine := loc.Range.Start.Line
		m.status = fmt.Sprintf("→ %s:%d", rel, targetLine+1)
		m.statusKind = "info"
		m.focus = regionEditor
		return m, m.editor.Focus()

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// Forward other messages to the editor when it has focus.
	if m.focus == regionEditor {
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		if m.editor.Value() != "" {
			m.dirty = true
		}
		return m, cmd
	}
	return m, nil
}

// refreshPalette refilters paletteItems based on paletteQuery.
func (m *ideModel) refreshPalette() {
	m.paletteItems = snippets.Search(m.paletteQuery)
}

// insertSelectedSnippet inserts the currently selected snippet body into the editor.
func (m *ideModel) insertSelectedSnippet() {
	if len(m.paletteItems) == 0 {
		return
	}
	if m.paletteSel < 0 || m.paletteSel >= len(m.paletteItems) {
		return
	}
	s := m.paletteItems[m.paletteSel]
	m.editor.InsertString(expandSnippet(s.Body))
	m.dirty = true
}

// expandSnippet strips tab-stop syntax from a snippet body, keeping defaults.
// ${1:default} → default, ${1:} → "", ${1} → "".
var reTabStopDefault = regexp.MustCompile(`\$\{(\d+):([^}]*)\}`)
var reTabStopBare = regexp.MustCompile(`\$\{\d+\}`)

func expandSnippet(body string) string {
	body = reTabStopDefault.ReplaceAllString(body, "$2")
	body = reTabStopBare.ReplaceAllString(body, "")
	return body
}

// currentWord returns the identifier prefix before the cursor and its start rune column.
func (m *ideModel) currentWord() (string, int) {
	col := m.editor.Column()
	line := lsp.LineAt(m.editor.Value(), m.editor.Line())
	runes := []rune(line)
	if col > len(runes) {
		col = len(runes)
	}
	start := col
	for start > 0 {
		r := runes[start-1]
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			start--
		} else {
			break
		}
	}
	return string(runes[start:col]), start
}

// acceptCompletion applies the selected completion item to the editor buffer.
func (m *ideModel) acceptCompletion() {
	if len(m.completeItems) == 0 || m.completeSel < 0 || m.completeSel >= len(m.completeItems) {
		return
	}
	it := m.completeItems[m.completeSel]
	doc := m.editor.Value()

	var edits []lsp.TextEdit
	if it.edit != nil {
		edits = append([]lsp.TextEdit{*it.edit}, it.addl...)
	} else {
		// Synthesize a TextEdit replacing [wordStart, cursor) with the insert text.
		row := m.editor.Line()
		line := lsp.LineAt(doc, row)
		startChar := lsp.EncodedColumn(line, m.completeWordStart, m.enc)
		endChar := lsp.EncodedColumn(line, m.editor.Column(), m.enc)
		synth := lsp.TextEdit{
			Range: lsp.Range{
				Start: lsp.Position{Line: row, Character: startChar},
				End:   lsp.Position{Line: row, Character: endChar},
			},
			NewText: it.insert,
		}
		edits = append([]lsp.TextEdit{synth}, it.addl...)
	}
	doc = applyEdits(doc, edits, m.enc)
	m.editor.SetValue(doc)
	m.dirty = true
	m.showComplete = false

	if m.client != nil {
		m.docVersion++
		uri := lsp.URI(filepath.Join(m.ws.Dir, m.openPath))
		_ = m.client.DidChange(uri, doc, m.docVersion)
	}
}

// handleKey dispatches key presses.
func (m *ideModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	keyStr := msg.String()

	// --- Completion popup overlay ---
	if m.showComplete {
		switch keyStr {
		case "esc":
			m.showComplete = false
			return m, nil
		case "up":
			if m.completeSel > 0 {
				m.completeSel--
			}
			return m, nil
		case "down":
			if m.completeSel < len(m.completeItems)-1 {
				m.completeSel++
			}
			return m, nil
		case "tab", "enter":
			m.acceptCompletion()
			return m, nil
		default:
			// Any other key dismisses the popup and falls through to the editor.
			m.showComplete = false
		}
	}

	// --- Snippet palette overlay ---
	if m.showPalette {
		switch keyStr {
		case "esc":
			m.showPalette = false
			return m, nil
		case "up":
			if m.paletteSel > 0 {
				m.paletteSel--
			}
			return m, nil
		case "down":
			if m.paletteSel < len(m.paletteItems)-1 {
				m.paletteSel++
			}
			return m, nil
		case "enter":
			m.insertSelectedSnippet()
			m.showPalette = false
			return m, nil
		case "backspace":
			if len(m.paletteQuery) > 0 {
				// Trim last UTF-8 rune.
				_, size := utf8.DecodeLastRuneInString(m.paletteQuery)
				m.paletteQuery = m.paletteQuery[:len(m.paletteQuery)-size]
				m.refreshPalette()
				m.paletteSel = 0
			}
			return m, nil
		default:
			// Printable single rune → append to query.
			if utf8.RuneCountInString(keyStr) == 1 {
				r, _ := utf8.DecodeRuneInString(keyStr)
				if r >= 32 && r != 127 {
					m.paletteQuery += keyStr
					m.refreshPalette()
					m.paletteSel = 0
				}
			}
			return m, nil
		}
	}

	// Esc closes hover overlay (before API / other esc uses).
	if m.showHover && keyStr == "esc" {
		m.showHover = false
		return m, nil
	}

	// Esc closes the API reference panel (palette intercept already handled above).
	if m.showAPI && keyStr == "esc" {
		m.showAPI = false
		return m, nil
	}

	switch keyStr {
	case "ctrl+p":
		m.showPalette = true
		m.paletteQuery = ""
		m.refreshPalette()
		m.paletteSel = 0
		return m, nil

	case "ctrl+space":
		word, start := m.currentWord()
		m.completeWordStart = start

		// Build Ouvrier snippet items.
		var snipItems []completeItem
		var snips []snippets.Snippet
		if word == "" {
			snips = snippets.All()
		} else {
			snips = snippets.Search(word)
		}
		for _, s := range snips {
			snipItems = append(snipItems, completeItem{
				label:   "◇ " + s.Prefix + "  " + s.Title,
				insert:  expandSnippet(s.Body),
				snippet: true,
			})
		}
		m.completeItems = snipItems
		m.completeSel = 0
		m.showComplete = true

		// Also fire gopls completion if LSP is available.
		if m.client != nil {
			uri := lsp.URI(filepath.Join(m.ws.Dir, m.openPath))
			pos := m.cursorPos()
			cl := m.client
			ctx := m.ctx
			return m, func() tea.Msg {
				list, err := cl.Complete(ctx, uri, pos)
				if err != nil || list == nil {
					return completeMsg{}
				}
				items := make([]completeItem, 0, len(list.Items))
				for _, gi := range list.Items {
					insert := gi.InsertText
					if insert == "" {
						insert = gi.Label
					}
					var edit *lsp.TextEdit
					if gi.TextEdit != nil {
						e := *gi.TextEdit
						edit = &e
					}
					items = append(items, completeItem{
						label:  gi.Label,
						detail: gi.Detail,
						insert: insert,
						edit:   edit,
						addl:   gi.AdditionalTextEdits,
					})
				}
				return completeMsg{items: items}
			}
		}
		return m, nil

	case "ctrl+\\":
		m.showAPI = !m.showAPI
		return m, nil

	case "ctrl+k":
		if m.client == nil {
			return m, nil
		}
		uri := lsp.URI(filepath.Join(m.ws.Dir, m.openPath))
		pos := m.cursorPos()
		cl := m.client
		ctx := m.ctx
		return m, func() tea.Msg {
			h, err := cl.Hover(ctx, uri, pos)
			if err != nil || h == nil {
				return hoverMsg{text: ""}
			}
			text := renderHoverMarkdown(h.Contents.Value)
			return hoverMsg{text: text}
		}

	case "ctrl+]":
		if m.client == nil {
			return m, nil
		}
		uri := lsp.URI(filepath.Join(m.ws.Dir, m.openPath))
		pos := m.cursorPos()
		cl := m.client
		ctx := m.ctx
		return m, func() tea.Msg {
			locs, err := cl.Definition(ctx, uri, pos)
			if err != nil {
				return defMsg{}
			}
			return defMsg{locs: locs}
		}

	case "ctrl+t":
		if len(m.jumpStack) == 0 {
			return m, nil
		}
		jmp := m.jumpStack[len(m.jumpStack)-1]
		m.jumpStack = m.jumpStack[:len(m.jumpStack)-1]
		content, err := operate.ReadWorkerFile(m.ws, jmp.path)
		if err != nil {
			m.status = "jump-back: open " + jmp.path + ": " + err.Error()
			m.statusKind = "fail"
			return m, nil
		}
		m.openPath = jmp.path
		m.editor.SetValue(content)
		m.dirty = false
		if m.client != nil {
			uri := lsp.URI(filepath.Join(m.ws.Dir, jmp.path))
			_ = m.client.DidOpen(uri, "go", content, 1)
		}
		m.status = fmt.Sprintf("← %s:%d", jmp.path, jmp.row+1)
		m.statusKind = "info"
		m.focus = regionEditor
		return m, m.editor.Focus()

	case "ctrl+q":
		cl := m.client
		return m, tea.Sequence(
			func() tea.Msg {
				if cl != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_ = cl.Shutdown(ctx)
				}
				return nil
			},
			tea.Quit,
		)

	case "ctrl+c":
		return m, tea.Quit

	case "ctrl+s":
		// Save the current file.
		if m.ws.Dir != "" {
			if err := operate.WriteWorkerFile(m.ws, m.openPath, m.editor.Value()); err != nil {
				m.status = "save failed: " + err.Error()
				m.statusKind = "fail"
				return m, nil // keep dirty; do NOT audit a write that didn't happen
			}
			m.dirty = false
			if m.client != nil {
				uri := lsp.URI(filepath.Join(m.ws.Dir, m.openPath))
				_ = m.client.DidChange(uri, m.editor.Value(), 2)
				_ = m.client.DidSave(uri)
			}
		}
		m.status = "saved — auditing"
		m.statusKind = "running"
		return m, m.runAudit()

	case "ctrl+b":
		m.building = true
		m.status = "building..."
		m.statusKind = "running"
		return m, m.runBuild()

	case "tab":
		m.focus = (m.focus + 1) % 3
		if m.focus == regionEditor {
			return m, m.editor.Focus()
		}
		m.editor.Blur()
		return m, nil

	case "shift+tab":
		m.focus = (m.focus + 2) % 3
		if m.focus == regionEditor {
			return m, m.editor.Focus()
		}
		m.editor.Blur()
		return m, nil

	case "]d":
		if m.problemSel < len(m.problems)-1 {
			m.problemSel++
		}
		return m, nil

	case "[d":
		if m.problemSel > 0 {
			m.problemSel--
		}
		return m, nil

	case "r":
		// Retry LSP init when focus is not on the editor.
		if m.focus != regionEditor && m.goplsPath != "" {
			if m.client != nil {
				old := m.client
				m.client = nil
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_ = old.Shutdown(ctx)
				}()
			}
			goplsPath := m.goplsPath
			wsDir := m.ws.Dir
			openPath := m.openPath
			editorContent := m.editor.Value()
			m.lspStatus = "lsp: connecting..."
			return m, func() tea.Msg {
				root := moduleRoot(wsDir)
				client, err := lsp.New(m.ctx, goplsPath, root)
				if err != nil {
					return lspReadyMsg{err: err}
				}
				uri := lsp.URI(filepath.Join(wsDir, openPath))
				_ = client.DidOpen(uri, "go", editorContent, 1)
				return lspReadyMsg{client: client}
			}
		}
	}

	// Tree navigation.
	if m.focus == regionTree {
		switch keyStr {
		case "up":
			if m.treeSel > 0 {
				m.treeSel--
			}
			return m, nil
		case "down":
			if m.treeSel < len(m.tree)-1 {
				m.treeSel++
			}
			return m, nil
		case "enter", "right":
			return m.openTreeSelection()
		}
	}

	// Problems panel navigation.
	if m.focus == regionProblems {
		switch keyStr {
		case "up":
			if m.problemSel > 0 {
				m.problemSel--
			}
			return m, nil
		case "down":
			if m.problemSel < len(m.problems)-1 {
				m.problemSel++
			}
			return m, nil
		}
	}

	// Forward to editor when focused.
	if m.focus == regionEditor {
		var cmd tea.Cmd
		prevVal := m.editor.Value()
		m.editor, cmd = m.editor.Update(msg)
		if m.editor.Value() != prevVal {
			m.dirty = true
		}
		return m, cmd
	}
	return m, nil
}

// openTreeSelection opens the currently selected tree item in the editor.
func (m *ideModel) openTreeSelection() (tea.Model, tea.Cmd) {
	if len(m.tree) == 0 || m.treeSel >= len(m.tree) {
		return m, nil
	}
	item := m.tree[m.treeSel]
	if item.IsDir {
		return m, nil
	}
	content, err := operate.ReadWorkerFile(m.ws, item.Rel)
	if err != nil {
		return m, nil
	}
	m.openPath = item.Rel
	m.editor.SetValue(content)
	m.dirty = false

	// Notify LSP of the new open file.
	if m.client != nil {
		uri := lsp.URI(filepath.Join(m.ws.Dir, m.openPath))
		_ = m.client.DidOpen(uri, "go", content, 1)
	}
	m.focus = regionEditor
	return m, m.editor.Focus()
}

// rebuildProblems reconstructs the unified problems list from diags + audit.
func (m *ideModel) rebuildProblems() {
	var lspProbs []Problem
	for uri, diags := range m.diags {
		// Convert URI to relative path.
		absPath := uriToPath(uri)
		rel := absPath
		if m.ws.Dir != "" {
			if r, err := filepath.Rel(m.ws.Dir, absPath); err == nil {
				rel = r
			}
		}
		for _, d := range diags {
			lspProbs = append(lspProbs, Problem{
				Source:   "lsp",
				File:     rel,
				Line:     d.Range.Start.Line + 1,
				Col:      d.Range.Start.Character + 1,
				Severity: d.Severity,
				Message:  d.Message,
				Origin:   d.Source,
			})
		}
	}
	m.problems = mergeProblems(lspProbs, m.auditProbs)
	// Clamp selection.
	if m.problemSel >= len(m.problems) {
		m.problemSel = max(0, len(m.problems)-1)
	}
}

// runAudit returns a Cmd that runs the audit and returns an auditMsg.
func (m *ideModel) runAudit() tea.Cmd {
	ctx := m.ctx
	dir := m.ws.Dir
	return func() tea.Msg {
		report, err := operate.NewAuditRunner().Run(ctx, dir)
		return auditMsg{report: report, err: err}
	}
}

// runBuild returns a Cmd that builds the worker and returns a buildMsg.
func (m *ideModel) runBuild() tea.Cmd {
	ctx := m.ctx
	dir := m.ws.Dir
	auditPassed := m.auditPassed
	return func() tea.Msg {
		artifact, err := operate.BuildCoordinator{}.Build(
			ctx, "ide", dir, "", auditPassed, operate.ProgressWriter{Out: io.Discard, Err: io.Discard},
		)
		return buildMsg{artifact: artifact, err: err}
	}
}

// listenDiag returns a Cmd that waits for one diagnostic notification.
func listenDiag(c *lsp.Client) tea.Cmd {
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		p, ok := <-c.Diagnostics()
		if !ok {
			return nil
		}
		return diagMsg{p}
	}
}

// moduleRoot walks up from dir to find the nearest go.mod file.
func moduleRoot(dir string) string {
	cur := dir
	for {
		if _, err := os.Stat(filepath.Join(cur, "go.mod")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return dir
}

// renderHoverMarkdown lightly strips markdown fences and leading headings from
// hover content so it renders legibly in a plain-text overlay.
func renderHoverMarkdown(s string) string {
	var out []string
	inFence := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		// Strip leading markdown headings.
		trimmed := strings.TrimLeft(line, "#")
		if len(trimmed) < len(line) {
			line = strings.TrimSpace(trimmed)
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// uriToPath converts a file:// URI to an absolute filesystem path.
func uriToPath(uri string) string {
	const prefix = "file://"
	if len(uri) > len(prefix) && uri[:len(prefix)] == prefix {
		return uri[len(prefix):]
	}
	return uri
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
