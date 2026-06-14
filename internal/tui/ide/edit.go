package ide

import (
	"sort"
	"strings"

	"github.com/ArnaudGuiovanna/ouvrier/internal/lsp"
)

// applyEdits applies LSP text edits to doc (sorted by descending start so earlier
// offsets stay valid), honoring the encoding for the character axis.
func applyEdits(doc string, edits []lsp.TextEdit, enc lsp.PositionEncoding) string {
	if len(edits) == 0 {
		return doc
	}
	lines := strings.Split(doc, "\n")
	type off struct {
		start, end int
		text       string
	}
	offs := make([]off, 0, len(edits))
	for _, e := range edits {
		s := posToOffset(lines, e.Range.Start, enc)
		en := posToOffset(lines, e.Range.End, enc)
		if en < s {
			s, en = en, s
		}
		offs = append(offs, off{s, en, e.NewText})
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i].start > offs[j].start })
	b := []rune(doc)
	for _, o := range offs {
		if o.start < 0 {
			o.start = 0
		}
		if o.end > len(b) {
			o.end = len(b)
		}
		if o.start > len(b) {
			o.start = len(b)
		}
		b = append(b[:o.start], append([]rune(o.text), b[o.end:]...)...)
	}
	return string(b)
}

// posToOffset converts an LSP position to an absolute RUNE offset into the doc.
func posToOffset(lines []string, p lsp.Position, enc lsp.PositionEncoding) int {
	off := 0
	for i := 0; i < p.Line && i < len(lines); i++ {
		off += len([]rune(lines[i])) + 1 // +1 for the \n
	}
	var line string
	if p.Line < len(lines) {
		line = lines[p.Line]
	}
	off += lsp.RuneColumn(line, p.Character, enc)
	return off
}
