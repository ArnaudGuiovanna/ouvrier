package ide

import "sort"

// Problem is a unified diagnostic item surfaced in the Problems panel.
// It may originate from the LSP (gopls), an audit gate, or a code review.
type Problem struct {
	Source   string // "lsp" | "audit" | "review"
	File     string // worker-relative path
	Line     int    // 1-based
	Col      int    // 1-based
	Severity int    // 1=error 2=warn 3=info 4=hint
	Message  string
	Origin   string // e.g. "compiler", "gofmt"
}

// mergeProblems combines two slices of Problems and returns them sorted by
// severity (errors first) then by file and line number.
func mergeProblems(lspP []Problem, auditP []Problem) []Problem {
	all := make([]Problem, 0, len(lspP)+len(auditP))
	all = append(all, lspP...)
	all = append(all, auditP...)
	sort.SliceStable(all, func(i, j int) bool {
		pi, pj := all[i], all[j]
		if pi.Severity != pj.Severity {
			return pi.Severity < pj.Severity
		}
		if pi.File != pj.File {
			return pi.File < pj.File
		}
		return pi.Line < pj.Line
	})
	return all
}

// severityColor returns the hex color for a given LSP severity level.
func severityColor(sev int) string {
	switch sev {
	case 1:
		return diagErrorHex
	case 2:
		return diagWarnHex
	case 3:
		return diagInfoHex
	case 4:
		return diagHintHex
	default:
		return overlay2Hex
	}
}

// severityGlyph returns a single-character glyph for a given LSP severity level.
func severityGlyph(sev int) string {
	switch sev {
	case 1:
		return "●"
	case 2:
		return "▲"
	case 3:
		return "●"
	case 4:
		return "○"
	default:
		return "●"
	}
}
