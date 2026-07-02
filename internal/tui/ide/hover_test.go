package ide

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
	"github.com/ArnaudGuiovanna/ouvrier/internal/operate"
)

// makeIDEModel builds a ready ideModel for hover/definition tests.
func makeIDEModel(t *testing.T) *ideModel {
	t.Helper()
	dir := writeIDEWorker(t)
	ws := makeIDEWorkspace(t, dir)
	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return updated.(*ideModel)
}

// TestHoverMsgShowsPopover verifies that a hoverMsg with non-empty text sets
// showHover=true and that the rendered frame contains the hover text.
func TestHoverMsgShowsPopover(t *testing.T) {
	m := makeIDEModel(t)

	updated, _ := m.Update(hoverMsg{text: "func main()"})
	m = updated.(*ideModel)

	if !m.showHover {
		t.Fatal("showHover should be true after hoverMsg with text")
	}
	if m.hoverText != "func main()" {
		t.Fatalf("hoverText = %q, want %q", m.hoverText, "func main()")
	}

	// The rendered view should contain the hover text.
	content := m.View().Content
	if !strings.Contains(content, "func main()") {
		t.Fatal("rendered view does not contain hover text 'func main()'")
	}
}

// TestHoverEmptyHides verifies that hoverMsg{text:""} clears the overlay.
func TestHoverEmptyHides(t *testing.T) {
	m := makeIDEModel(t)

	// First show it.
	updated, _ := m.Update(hoverMsg{text: "something"})
	m = updated.(*ideModel)
	if !m.showHover {
		t.Fatal("precondition: showHover should be true")
	}

	// Now hide it with empty text.
	updated, _ = m.Update(hoverMsg{text: ""})
	m = updated.(*ideModel)
	if m.showHover {
		t.Fatal("showHover should be false after hoverMsg with empty text")
	}
}

// TestDefinitionOpensFileInWorkspace verifies that a defMsg with a Location
// inside the workspace opens that file and pushes the current location onto
// the jump stack.
func TestDefinitionOpensFileInWorkspace(t *testing.T) {
	// Create a workspace with two files.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "helper.go"), []byte("package main\n\nfunc helper() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/worker\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pip.yaml"), []byte("name: worker\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ouvrier.worker.json"), []byte(`{"name":"worker","main":"main.go"}`), 0o644)

	ws := operate.Workspace{
		Dir:      dir,
		Name:     "worker",
		MainPath: filepath.Join(dir, "main.go"),
	}
	m := newIDEModel(context.Background(), IDEOptions{Workspace: ws, GoplsPath: ""})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*ideModel)

	// Current open path is main.go.
	if m.openPath != "main.go" {
		t.Fatalf("initial openPath = %q, want main.go", m.openPath)
	}

	// Feed a defMsg pointing to helper.go line 0.
	helperURI := lsp.URI(filepath.Join(dir, "helper.go"))
	updated, _ = m.Update(defMsg{locs: []lsp.Location{
		{
			URI:   helperURI,
			Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}},
		},
	}})
	m = updated.(*ideModel)

	// openPath should now be helper.go.
	if !strings.Contains(m.openPath, "helper.go") {
		t.Fatalf("openPath = %q after defMsg, want to contain 'helper.go'", m.openPath)
	}

	// Jump stack should have 1 entry (the previous main.go position).
	if len(m.jumpStack) != 1 {
		t.Fatalf("jumpStack len = %d, want 1", len(m.jumpStack))
	}
	if m.jumpStack[0].path != "main.go" {
		t.Fatalf("jumpStack[0].path = %q, want main.go", m.jumpStack[0].path)
	}
}
