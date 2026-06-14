package ide

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

func TestExpandSnippet(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			input: `ovr.Pipe("${1:goal}", ovr.Output[${2:Result}]())`,
			want:  `ovr.Pipe("goal", ovr.Output[Result]())`,
		},
		{
			input: `x${1:}y`,
			want:  `xy`,
		},
		{
			input: `ovr.Tool("${1:name}", ${2:fn},\n\tovr.ReadOnly(),\n\tovr.Describe("${3:what it does}"),\n)`,
			want:  `ovr.Tool("name", fn,\n\tovr.ReadOnly(),\n\tovr.Describe("what it does"),\n)`,
		},
		{
			input: `bare${1}end`,
			want:  `bareend`,
		},
	}
	for _, c := range cases {
		got := expandSnippet(c.input)
		if got != c.want {
			t.Errorf("expandSnippet(%q)\n  got  %q\n  want %q", c.input, got, c.want)
		}
	}
}

func makeTestModel(t *testing.T) *ideModel {
	t.Helper()
	dir := writeIDEWorker(t)
	ws := operate.Workspace{
		Dir:      dir,
		Name:     "worker",
		MainPath: dir + "/main.go",
	}
	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ideModel)
	return m
}

func TestSnippetPaletteOpensAndInserts(t *testing.T) {
	m := makeTestModel(t)

	// Set a known editor value.
	m.editor.SetValue("package main\n")

	// Open palette via ctrl+p key.
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	m = updated.(*ideModel)

	if !m.showPalette {
		t.Fatal("showPalette should be true after ctrl+p")
	}
	if len(m.paletteItems) == 0 {
		t.Fatal("paletteItems should not be empty after opening palette")
	}

	// Find the ovr-tool snippet index.
	toolIdx := -1
	for i, s := range m.paletteItems {
		if s.Prefix == "ovr-tool" {
			toolIdx = i
			break
		}
	}
	if toolIdx == -1 {
		t.Fatal("could not find ovr-tool snippet in paletteItems")
	}

	// Set selection to ovr-tool.
	m.paletteSel = toolIdx

	// Press enter to insert.
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*ideModel)

	if m.showPalette {
		t.Fatal("palette should be closed after enter")
	}
	if !m.dirty {
		t.Fatal("dirty should be true after inserting snippet")
	}
	val := m.editor.Value()
	if !strings.Contains(val, "ovr.Tool") {
		t.Fatalf("editor value should contain 'ovr.Tool' after inserting ovr-tool snippet; got: %q", val)
	}
}

func TestSnippetPaletteFilter(t *testing.T) {
	m := makeTestModel(t)

	// Open palette.
	m.showPalette = true
	m.paletteQuery = ""
	m.refreshPalette()
	allCount := len(m.paletteItems)

	// Type 'tool' to filter.
	m.paletteQuery = "tool"
	m.refreshPalette()
	if len(m.paletteItems) >= allCount {
		t.Fatalf("after filtering with 'tool', expected fewer items than %d, got %d", allCount, len(m.paletteItems))
	}
	found := false
	for _, s := range m.paletteItems {
		if s.Prefix == "ovr-tool" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("ovr-tool should appear after filtering with 'tool'")
	}
}

func TestSnippetPaletteEscCloses(t *testing.T) {
	m := makeTestModel(t)

	m.showPalette = true
	m.refreshPalette()

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = updated.(*ideModel)

	if m.showPalette {
		t.Fatal("showPalette should be false after esc")
	}
}

func TestAPIPanelRenders(t *testing.T) {
	m := makeTestModel(t)
	m.showAPI = true

	view := m.View()
	content := view.Content
	if !strings.Contains(content, "Ouvrier API") {
		t.Errorf("view should contain 'Ouvrier API' when showAPI is true; got: %q", content[:min(len(content), 200)])
	}
	if !strings.Contains(content, "ovr-tool") {
		t.Errorf("view should contain snippet prefix 'ovr-tool' in API panel; got: %q", content[:min(len(content), 200)])
	}
}

func TestCtrlBackslashTogglesAPI(t *testing.T) {
	m := makeTestModel(t)

	if m.showAPI {
		t.Fatal("showAPI should start false")
	}

	// Toggle on.
	updated, _ := m.Update(tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl})
	m = updated.(*ideModel)
	if !m.showAPI {
		t.Fatal("showAPI should be true after ctrl+\\")
	}

	// Toggle off.
	updated, _ = m.Update(tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl})
	m = updated.(*ideModel)
	if m.showAPI {
		t.Fatal("showAPI should be false after second ctrl+\\")
	}
}
