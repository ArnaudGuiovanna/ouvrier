package ide

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
)

func TestCompletionSnippetsWithoutGopls(t *testing.T) {
	m := makeTestModel(t)
	// client is nil (no gopls)

	// Set editor content.
	m.editor.SetValue("package main\nfunc main(){\n\tovr.\n}\n")

	// Trigger ctrl+space.
	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Mod: tea.ModCtrl})
	m = updated.(*ideModel)

	if !m.showComplete {
		t.Fatal("showComplete should be true after ctrl+space")
	}
	if len(m.completeItems) == 0 {
		t.Fatal("completeItems should not be empty after ctrl+space with no gopls")
	}
	// All items should be snippet items (no gopls).
	for _, it := range m.completeItems {
		if !it.snippet {
			t.Errorf("item %q should be a snippet item when client==nil", it.label)
		}
	}

	// Find an item that inserts something with "ovr." prefix.
	found := -1
	for i, it := range m.completeItems {
		if strings.Contains(it.insert, "ovr.") {
			found = i
			break
		}
	}
	if found == -1 {
		t.Fatal("no completion item contains 'ovr.' in its insert text")
	}

	// Accept that item.
	m.completeSel = found
	m.acceptCompletion()

	if m.showComplete {
		t.Fatal("showComplete should be false after acceptCompletion")
	}
	if !m.dirty {
		t.Fatal("dirty should be true after acceptCompletion")
	}
	val := m.editor.Value()
	if !strings.Contains(val, "ovr.") {
		t.Fatalf("editor value should contain 'ovr.' after accepting snippet; got: %q", val)
	}
}

func TestCompletionCtrlSpaceOpensPopup(t *testing.T) {
	m := makeTestModel(t)
	m.editor.SetValue("package main\n")

	if m.showComplete {
		t.Fatal("showComplete should start false")
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: ' ', Mod: tea.ModCtrl})
	m = updated.(*ideModel)

	if !m.showComplete {
		t.Fatal("showComplete should be true after ctrl+space")
	}
	if len(m.completeItems) == 0 {
		t.Fatal("completeItems should not be empty")
	}
}

func TestCompletionEscCloses(t *testing.T) {
	m := makeTestModel(t)
	m.showComplete = true
	m.completeItems = []completeItem{{label: "test", insert: "test", snippet: true}}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(*ideModel)

	if m.showComplete {
		t.Fatal("showComplete should be false after esc")
	}
}

func TestCompletionUpDownNavigation(t *testing.T) {
	m := makeTestModel(t)
	m.showComplete = true
	m.completeSel = 0
	m.completeItems = []completeItem{
		{label: "alpha", insert: "alpha", snippet: true},
		{label: "beta", insert: "beta", snippet: true},
		{label: "gamma", insert: "gamma", snippet: true},
	}

	// Down moves selection.
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(*ideModel)
	if m.completeSel != 1 {
		t.Fatalf("completeSel = %d after down, want 1", m.completeSel)
	}

	// Down again.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(*ideModel)
	if m.completeSel != 2 {
		t.Fatalf("completeSel = %d after 2nd down, want 2", m.completeSel)
	}

	// Down at end doesn't overflow.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m = updated.(*ideModel)
	if m.completeSel != 2 {
		t.Fatalf("completeSel = %d after down at end, want 2", m.completeSel)
	}

	// Up goes back.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(*ideModel)
	if m.completeSel != 1 {
		t.Fatalf("completeSel = %d after up, want 1", m.completeSel)
	}
}

func TestAcceptGoplsTextEdit(t *testing.T) {
	m := makeTestModel(t)
	doc := "package main\n\nfunc main() {\n\tfmt.Prntln()\n}\n"
	m.editor.SetValue(doc)
	m.enc = lsp.EncodingUTF8

	// Construct a completeItem with a TextEdit replacing "Prntln" with "Println".
	// "Prntln" is on line 3 at character 5.
	edit := lsp.TextEdit{
		Range: lsp.Range{
			Start: lsp.Position{Line: 3, Character: 5},
			End:   lsp.Position{Line: 3, Character: 11},
		},
		NewText: "Println",
	}
	m.showComplete = true
	m.completeItems = []completeItem{
		{
			label:  "Println",
			detail: "func Println",
			insert: "Println",
			edit:   &edit,
		},
	}
	m.completeSel = 0

	m.acceptCompletion()

	if m.showComplete {
		t.Fatal("showComplete should be false after acceptCompletion")
	}
	if !m.dirty {
		t.Fatal("dirty should be true after acceptCompletion")
	}
	val := m.editor.Value()
	if !strings.Contains(val, "Println") {
		t.Errorf("editor should contain 'Println' after gopls edit; got: %q", val)
	}
	if strings.Contains(val, "Prntln") {
		t.Errorf("editor should not contain 'Prntln' after edit; got: %q", val)
	}
}

func TestCompleteMsgMergesItems(t *testing.T) {
	m := makeTestModel(t)
	m.showComplete = true
	m.completeItems = []completeItem{
		{label: "◇ ovr-tool  ovr.Tool", insert: "ovr.Tool()", snippet: true},
	}

	// Simulate a completeMsg arriving with gopls items.
	updated, _ := m.Update(completeMsg{items: []completeItem{
		{label: "Println", detail: "func Println", insert: "Println"},
		{label: "Printf", detail: "func Printf", insert: "Printf"},
	}})
	m = updated.(*ideModel)

	if len(m.completeItems) != 3 {
		t.Fatalf("expected 3 items after merge, got %d", len(m.completeItems))
	}
	// Dedup: sending the same label twice should not add it again.
	updated, _ = m.Update(completeMsg{items: []completeItem{
		{label: "Println", detail: "func Println", insert: "Println"},
	}})
	m = updated.(*ideModel)
	if len(m.completeItems) != 3 {
		t.Fatalf("expected 3 items after dedup, got %d", len(m.completeItems))
	}
}

func TestCompletionViewRendersOverlay(t *testing.T) {
	m := makeTestModel(t)
	m.showComplete = true
	m.completeItems = []completeItem{
		{label: "◇ ovr-tool  ovr.Tool", insert: "ovr.Tool()", snippet: true},
		{label: "Println", detail: "builtin", insert: "Println"},
	}
	m.completeSel = 0

	view := m.View()
	content := view.Content
	if !strings.Contains(content, "tab insert") {
		t.Errorf("view should contain completion hint 'tab insert'; got excerpt: %q",
			content[:min(len(content), 300)])
	}
}
