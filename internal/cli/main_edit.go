package cli

import (
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
)

// ErrMainEdit is returned when ouvrier add cannot safely edit main.go.
var ErrMainEdit = errors.New("main.go edit failed")

// addImport ensures src has the supplied import path inside its `import (
// ... )` block. If the import is already present, src is returned unchanged.
// If main.go uses a single-line `import "x"` form, the block is rewritten to
// a parenthesised group containing both imports. Returns (src, false) when no
// import statement is present, signalling that callers should refuse the edit.
func addImport(src, path string) (string, bool) {
	quoted := `"` + path + `"`
	if importAlreadyPresent(src, quoted) {
		return src, true
	}

	// Parenthesised import block: `import (\n\t...\n)`.
	if openIdx := strings.Index(src, "import ("); openIdx >= 0 {
		closeIdx := strings.Index(src[openIdx:], ")")
		if closeIdx < 0 {
			return src, false
		}
		closeAbs := openIdx + closeIdx
		// Insert a new line just before the closing paren, preserving tab
		// indentation. gofmt aligns the block at write-time.
		insertion := "\t" + quoted + "\n"
		return src[:closeAbs] + insertion + src[closeAbs:], true
	}

	// Single-line import: `import "x"`.
	const singleImport = "import \""
	if idx := strings.Index(src, singleImport); idx >= 0 {
		end := strings.IndexByte(src[idx:], '\n')
		if end < 0 {
			return src, false
		}
		original := src[idx : idx+end]
		// Extract the existing import literal.
		rest := strings.TrimPrefix(original, "import ")
		rewritten := "import (\n\t" + rest + "\n\t" + quoted + "\n)"
		return src[:idx] + rewritten + src[idx+end:], true
	}

	return src, false
}

// importAlreadyPresent reports whether src already references the given
// quoted import literal inside its import declarations.
func importAlreadyPresent(src, quoted string) bool {
	idx := strings.Index(src, "import")
	if idx < 0 {
		return false
	}
	// Scan up to the end of the import declaration. For a parenthesised
	// block we cap at the matching ')'; for the single-line form we cap at
	// the newline.
	tail := src[idx:]
	openParen := strings.Index(tail, "(")
	closeParen := strings.Index(tail, ")")
	end := len(tail)
	if openParen >= 0 && closeParen > openParen {
		end = closeParen
	} else if nl := strings.IndexByte(tail, '\n'); nl >= 0 {
		end = nl
	}
	return strings.Contains(tail[:end], quoted)
}

// projectModulePath reads go.mod from the project root and returns the
// declared module path. The empty string is returned if go.mod is missing or
// malformed; callers should treat that as a non-fatal degradation.
func projectModulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "module "))
		}
	}
	return ""
}

// loadMainGo reads main.go from the project root or returns a helpful error if
// it is missing.
func loadMainGo(root string) (string, []byte, error) {
	path := filepath.Join(root, "main.go")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil, fmt.Errorf("%w: %s not found", ErrMainEdit, path)
		}
		return path, nil, fmt.Errorf("%w: read main.go: %w", ErrMainEdit, err)
	}
	return path, data, nil
}

// writeMainGo overwrites main.go with the supplied bytes, formatting the
// result with gofmt. If gofmt rejects the bytes the file is left untouched and
// an error is returned.
func writeMainGo(path string, source []byte) error {
	formatted, err := format.Source(source)
	if err != nil {
		return fmt.Errorf("%w: gofmt rejected the edited main.go: %w", ErrMainEdit, err)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return fmt.Errorf("%w: write main.go: %w", ErrMainEdit, err)
	}
	return nil
}

// insertAfterLine returns src with insertion spliced in immediately after the
// line containing anchor. The caller is responsible for providing trailing
// newlines on insertion. Returns ok=false if anchor was not found.
func insertAfterLine(src, anchor, insertion string) (string, bool) {
	idx := strings.Index(src, anchor)
	if idx < 0 {
		return src, false
	}
	// Find end of line containing the anchor.
	nl := strings.IndexByte(src[idx:], '\n')
	if nl < 0 {
		return src + "\n" + insertion, true
	}
	cut := idx + nl + 1
	return src[:cut] + insertion + src[cut:], true
}

// insertBeforeLine returns src with insertion spliced in immediately before
// the line containing anchor. Returns ok=false if anchor was not found.
func insertBeforeLine(src, anchor, insertion string) (string, bool) {
	idx := strings.Index(src, anchor)
	if idx < 0 {
		return src, false
	}
	// Find start of line containing the anchor.
	lineStart := strings.LastIndexByte(src[:idx], '\n')
	if lineStart < 0 {
		lineStart = 0
	} else {
		lineStart++
	}
	return src[:lineStart] + insertion + src[lineStart:], true
}

// firstTerminalAnchor reports the byte offset of the first ovr.Reply(,
// ovr.Push( or ovr.Sink( call. Returns -1 when none of those appear.
func firstTerminalAnchor(src string) (int, string) {
	candidates := []string{"ovr.Reply(", "ovr.Push(", "ovr.Sink("}
	best := -1
	var bestKey string
	for _, key := range candidates {
		if idx := strings.Index(src, key); idx >= 0 && (best < 0 || idx < best) {
			best = idx
			bestKey = key
		}
	}
	return best, bestKey
}

// findFirstPipeBlock returns the byte offsets [open, close] (both inclusive
// of the parenthesis) of the first `ovr.Pipe(` block by tracking parenthesis
// depth. Strings and comments inside the block are not interpreted; that is
// good enough for the conservative editing this command performs on generated
// projects.
func findFirstPipeBlock(src string) (open, close int, ok bool) {
	idx := strings.Index(src, "ovr.Pipe(")
	if idx < 0 {
		return 0, 0, false
	}
	openParen := idx + len("ovr.Pipe(") - 1
	depth := 0
	i := openParen
	inString := false
	var stringQuote byte
	for i < len(src) {
		c := src[i]
		if inString {
			switch c {
			case '\\':
				if i+1 < len(src) {
					i += 2
					continue
				}
			case stringQuote:
				inString = false
			}
			i++
			continue
		}
		switch c {
		case '"', '`':
			inString = true
			stringQuote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return openParen, i, true
			}
		}
		i++
	}
	return 0, 0, false
}

// firstPipeLineIndent returns the indentation prefix of the first line in the
// first ovr.Pipe block that already contains an option call (e.g. ovr.Model).
// This lets new options align with the existing block style. Returns "" if no
// option line is found.
func firstPipeLineIndent(src string, open, close int) string {
	if open <= 0 || close <= open || close >= len(src) {
		return ""
	}
	body := src[open+1 : close]
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.HasPrefix(trimmed, "ovr.") {
			continue
		}
		end := 0
		for end < len(line) && (line[end] == ' ' || line[end] == '\t') {
			end++
		}
		if end == 0 {
			return "\t\t\t"
		}
		return line[:end]
	}
	return ""
}

// appendPipeOption inserts `line` (without trailing newline) just before the
// closing parenthesis of the first ovr.Pipe block. The line is indented to
// match the existing block's options. Returns the new source and true on
// success; false if no Pipe block was detected.
func appendPipeOption(src, line string) (string, bool) {
	open, close, ok := findFirstPipeBlock(src)
	if !ok {
		return src, false
	}
	indent := firstPipeLineIndent(src, open, close)
	if indent == "" {
		indent = "\t\t\t"
	}
	// Walk backwards from close to find the position right after the last
	// non-whitespace character so we can place the new option on its own
	// line.
	insertAt := close
	for insertAt > open+1 {
		c := src[insertAt-1]
		if c == ' ' || c == '\t' || c == '\n' {
			insertAt--
			continue
		}
		break
	}
	// Ensure the previous line ends with a comma so Go accepts the new entry.
	prefix := src[:insertAt]
	suffix := src[insertAt:]
	addComma := ""
	if !strings.HasSuffix(prefix, ",") {
		addComma = ","
	}
	insertion := addComma + "\n" + indent + line + ","
	outdent := indent
	if len(outdent) > 0 {
		outdent = outdent[:len(outdent)-1]
	}
	return prefix + insertion + "\n" + outdent + suffix, true
}
