package lsp

import (
	"strings"
	"unicode/utf16"
)

// RuneColumn converts an LSP character offset on a line into a 0-based rune
// column, honoring the negotiated encoding; clamps to len([]rune(line)).
func RuneColumn(line string, character int, enc PositionEncoding) int {
	if character <= 0 {
		return 0
	}
	runes := []rune(line)
	if enc == EncodingUTF8 {
		b := 0
		for i, r := range runes {
			rb := len(string(r))
			if b+rb > character {
				return i
			}
			b += rb
			if b == character {
				return i + 1
			}
		}
		return len(runes)
	}
	u := 0
	for i, r := range runes {
		w := len(utf16.Encode([]rune{r}))
		if u+w > character {
			return i
		}
		u += w
		if u == character {
			return i + 1
		}
	}
	return len(runes)
}

// LineAt returns the row-th line of doc (split on \n), clamped to "".
func LineAt(doc string, row int) string {
	if row < 0 {
		return ""
	}
	lines := strings.Split(doc, "\n")
	if row >= len(lines) {
		return ""
	}
	return lines[row]
}
