package ide

import (
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
)

func TestApplyEditsReplacesRange(t *testing.T) {
	doc := "hello world\n"
	edits := []lsp.TextEdit{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 0, Character: 6},
				End:   lsp.Position{Line: 0, Character: 11},
			},
			NewText: "Go",
		},
	}
	got := applyEdits(doc, edits, lsp.EncodingUTF8)
	want := "hello Go\n"
	if got != want {
		t.Errorf("applyEdits = %q, want %q", got, want)
	}
}

func TestApplyEditsTwoEdits(t *testing.T) {
	// Two edits: one near top (like an import insert at line 0), one mid-doc.
	doc := "package main\n\nfunc main() {\n\tfmt.Println()\n}\n"
	// Edit 1: insert "import \"fmt\"\n" after line 0 (at start of line 1)
	// Edit 2: replace "Println" with "Printf"
	edits := []lsp.TextEdit{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 1, Character: 0},
				End:   lsp.Position{Line: 1, Character: 0},
			},
			NewText: "import \"fmt\"\n",
		},
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 3, Character: 5},
				End:   lsp.Position{Line: 3, Character: 12},
			},
			NewText: "Printf",
		},
	}
	got := applyEdits(doc, edits, lsp.EncodingUTF8)
	if !strings.Contains(got, "import \"fmt\"") {
		t.Errorf("result missing import: %q", got)
	}
	if !strings.Contains(got, "Printf") {
		t.Errorf("result missing Printf: %q", got)
	}
	if strings.Contains(got, "Println") {
		t.Errorf("result still contains Println: %q", got)
	}
}

func TestApplyEditsMultibyteLine(t *testing.T) {
	// Line 0 contains a multibyte rune; edit on line 1 must land correctly.
	doc := "// é\nfoo bar\n"
	// Replace "bar" on line 1 (UTF-8: "bar" starts at character 4).
	edits := []lsp.TextEdit{
		{
			Range: lsp.Range{
				Start: lsp.Position{Line: 1, Character: 4},
				End:   lsp.Position{Line: 1, Character: 7},
			},
			NewText: "baz",
		},
	}
	got := applyEdits(doc, edits, lsp.EncodingUTF8)
	want := "// é\nfoo baz\n"
	if got != want {
		t.Errorf("applyEdits multibyte = %q, want %q", got, want)
	}
}

func TestApplyEditsNoEdits(t *testing.T) {
	doc := "unchanged\n"
	got := applyEdits(doc, nil, lsp.EncodingUTF8)
	if got != doc {
		t.Errorf("applyEdits with nil edits changed doc: got %q", got)
	}
}
